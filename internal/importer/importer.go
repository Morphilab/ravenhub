// Package importer applies a declarative seed YAML to the database
// (idempotente, con expansión ${ENV} para los secretos —).
package importer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"ravenhub/internal/adapter"
	"ravenhub/internal/model"
	"ravenhub/internal/render"
	"ravenhub/internal/store"
)

type seedFile struct {
	Projects []seedProject `yaml:"projects"`
}

type seedProject struct {
	Name         string         `yaml:"name"`
	Options      seedOptions    `yaml:"options"`
	Tokens       []seedToken    `yaml:"tokens"`
	Accounts     []seedAccount  `yaml:"accounts"`
	Destinations []seedDest     `yaml:"destinations"`
	Groups       []seedGroup    `yaml:"groups"`
	RoutingRules []seedRouting  `yaml:"routing_rules"`
	Templates    []seedTemplate `yaml:"templates"`
}

// seedOptions captura un mapping YAML como JSON. json.RawMessage no sirve
// directamente: yaml.v3 solo decodifica strings en []byte, así que el
// `options: {"ttl": {...}}` documentado en seed.example.yaml rompía el import
// entero ("cannot unmarshal !!map into json.RawMessage") — las options del
// seed eran inaplicables.
type seedOptions struct{ raw json.RawMessage }

func (o *seedOptions) UnmarshalYAML(node *yaml.Node) error {
	if node == nil || node.Tag == "!!null" {
		o.raw = nil
		return nil
	}
	var v any
	if err := node.Decode(&v); err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	o.raw = b
	return nil
}

// json returns the declared options as JSON (nil when absent).
func (o seedOptions) json() json.RawMessage { return o.raw }

// seedRouting declara una regla de enrutamiento por tags/prioridad.
type seedRouting struct {
	Name           string         `yaml:"name"`
	MatchTags      []string       `yaml:"match_tags"`
	MatchPriority  string         `yaml:"match_priority"`
	MatchPayload   map[string]any `yaml:"match_payload"`
	Destinations   []string       `yaml:"destinations"`
	StopProcessing bool           `yaml:"stop_processing"`
	Enabled        *bool          `yaml:"enabled"`  // default true
	Priority       int            `yaml:"priority"` // orden: menor = primero (default 100)
}

type seedToken struct {
	Name   string `yaml:"name"`
	Scopes struct {
		Destinations []string `yaml:"destinations"`
		MaxPriority  string   `yaml:"max_priority"`
	} `yaml:"scopes"`
}

type seedAccount struct {
	Label   string         `yaml:"label"`
	Channel string         `yaml:"channel"`
	Creds   map[string]any `yaml:"creds"`
	Limits  map[string]any `yaml:"limits"`
}

type seedDest struct {
	Name       string         `yaml:"name"`
	Account    string         `yaml:"account"`
	Target     map[string]any `yaml:"target"`
	QuietHours map[string]any `yaml:"quiet_hours"`
}

type seedGroup struct {
	Name         string   `yaml:"name"`
	Destinations []string `yaml:"destinations"`
}

type seedTemplate struct {
	Name string `yaml:"name"`
	Body string `yaml:"body"`
}

// minúsculas incluidas: el mecanismo es fail-fast y una ${var_minuscula}
// dejada literal en silencio era justo lo contrario de su propósito
var envRe = regexp.MustCompile(`\$\{([A-Za-z0-9_]+)\}`)

// Import applies the seed file. Idempotente: re-importar actualiza
// cuentas/destinos/grupos/plantillas/reglas y las options del proyecto, y
// sincroniza los scopes de los tokens existentes (el secreto solo se genera al
// crear). Las reglas en DB no declaradas en el seed se conservan (con aviso).
//
// La expansión ${ENV} se hace sobre el ÁRBOL PARSEADO (creds, limits,
// target, quiet_hours y el body de plantillas) y es ESTRICTA: una variable
// referenciada y ausente aborta el import ANTES de tocar la DB — re-importar
// con el entorno incompleto sobrescribía en claro creds que funcionaban
// (upsert) en lugar de fallar rápido.
func Import(ctx context.Context, st *store.Store, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("leyendo seed: %w", err)
	}
	var seed seedFile
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true) // un typo ('destinatios:') no puede pasar en silencio
	if err := dec.Decode(&seed); err != nil {
		return fmt.Errorf("YAML inválido: %w", err)
	}
	if len(seed.Projects) == 0 {
		return fmt.Errorf("el seed no tiene projects")
	}

	var missing []string
	seenMissing := map[string]bool{}
	expand := func(v any) any {
		var walk func(any) any
		walk = func(v any) any {
			switch t := v.(type) {
			case map[string]any:
				out := map[string]any{}
				for k, e := range t {
					out[k] = walk(e)
				}
				return out
			case []any:
				out := make([]any, 0, len(t))
				for _, e := range t {
					out = append(out, walk(e))
				}
				return out
			case string:
				return envRe.ReplaceAllStringFunc(t, func(m string) string {
					name := envRe.FindStringSubmatch(m)[1]
					val, ok := os.LookupEnv(name)
					if !ok && !seenMissing[name] {
						seenMissing[name] = true
						missing = append(missing, name)
					}
					return val
				})
			default:
				return v
			}
		}
		return walk(v)
	}
	for i := range seed.Projects {
		sp := &seed.Projects[i]
		for j := range sp.Accounts {
			a := &sp.Accounts[j]
			a.Creds, _ = expand(a.Creds).(map[string]any)
			a.Limits, _ = expand(a.Limits).(map[string]any)
		}
		for j := range sp.Destinations {
			d := &sp.Destinations[j]
			d.Target, _ = expand(d.Target).(map[string]any)
			d.QuietHours, _ = expand(d.QuietHours).(map[string]any)
		}
		for j := range sp.Templates {
			if b, ok := expand(sp.Templates[j].Body).(string); ok {
				sp.Templates[j].Body = b
			}
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("variables de entorno ausentes referenciadas por el seed: %s — defínelas en .env (el re-import con entorno incompleto NO debe vaciar credenciales que funcionan)", strings.Join(missing, ", "))
	}

	// Pre-flight (antes de tocar la DB): options y reglas válidas — un typo en
	// el seed no puede aplicarse a medias ni ignorarse en silencio.
	for _, sp := range seed.Projects {
		if err := model.ValidateProjectOptions(sp.Options.json()); err != nil {
			return fmt.Errorf("proyecto %s: %w", sp.Name, err)
		}
		for _, a := range sp.Accounts {
			if !adapter.Supported(a.Channel) {
				return fmt.Errorf("proyecto %s: cuenta %q: canal no soportado %q", sp.Name, a.Label, a.Channel)
			}
		}
		for _, d := range sp.Destinations {
			qh, _ := json.Marshal(d.QuietHours)
			if _, err := render.ParseQuietHours(qh); err != nil {
				return fmt.Errorf("proyecto %s: destino %q: %w", sp.Name, d.Name, err)
			}
		}
		for _, r := range sp.RoutingRules {
			if strings.TrimSpace(r.Name) == "" {
				return fmt.Errorf("proyecto %s: regla de enrutamiento sin 'name'", sp.Name)
			}
			if len(r.Destinations) == 0 {
				return fmt.Errorf("proyecto %s: regla %q sin 'destinations'", sp.Name, r.Name)
			}
			if r.MatchPriority != "" {
				if _, err := model.ParsePriority(r.MatchPriority); err != nil {
					return fmt.Errorf("proyecto %s: regla %q: match_priority inválido: %w", sp.Name, r.Name, err)
				}
			}
		}
	}

	for _, sp := range seed.Projects {
		p, err := upsertProject(ctx, st, sp)
		if err != nil {
			return err
		}

		for _, t := range sp.Tokens {
			created, err := st.EnsureProjectToken(ctx, p.ID, t.Name, t.Scopes.Destinations, t.Scopes.MaxPriority)
			if err != nil {
				return fmt.Errorf("token %s: %w", t.Name, err)
			}
			if created != "" {
				fmt.Printf("  🔑 TOKEN NUEVO (guárdalo, no se muestra de nuevo):\n     %s\n", created)
				continue
			}
			// token existente: los scopes declarados son autoritativos.
			changed, err := st.UpdateTokenScopes(ctx, p.ID, t.Name, t.Scopes.Destinations, t.Scopes.MaxPriority)
			if err != nil {
				return fmt.Errorf("token %s: %w", t.Name, err)
			}
			if changed {
				fmt.Printf("  ↻ token %s: scopes actualizados\n", t.Name)
				st.InsertAudit(ctx, &p.ID, "seed", "token.scopes_updated", t.Name,
					map[string]any{"destinations": t.Scopes.Destinations, "max_priority": t.Scopes.MaxPriority})
			}
		}

		for _, a := range sp.Accounts {
			creds, _ := json.Marshal(a.Creds)
			limits, _ := json.Marshal(a.Limits)
			// un mapa nil serializa como "null" y fallaría recién al enviar
			// normalizar a objeto vacío para fallar rápido en validación
			if string(creds) == "null" {
				creds = []byte(`{}`)
			}
			if string(limits) == "null" {
				limits = []byte(`{}`)
			}
			if err := st.UpsertAccount(ctx, p.ID, a.Channel, a.Label, creds, limits); err != nil {
				return fmt.Errorf("cuenta %s: %w", a.Label, err)
			}
			fmt.Printf("  ✓ cuenta %s (%s)\n", a.Label, a.Channel)
		}

		for _, d := range sp.Destinations {
			acct, err := st.GetAccountByLabel(ctx, p.ID, d.Account)
			if err != nil {
				return fmt.Errorf("destino %s: cuenta %s: %w", d.Name, d.Account, err)
			}
			target, _ := json.Marshal(d.Target)
			qh, _ := json.Marshal(d.QuietHours)
			if err := st.UpsertDestination(ctx, p.ID, d.Name, acct.ID, target, qh); err != nil {
				return fmt.Errorf("destino %s: %w", d.Name, err)
			}
			fmt.Printf("  ✓ destino %s → %s\n", d.Name, d.Account)
		}

		for _, g := range sp.Groups {
			if err := st.UpsertGroup(ctx, p.ID, g.Name, g.Destinations); err != nil {
				return fmt.Errorf("grupo %s: %w", g.Name, err)
			}
			fmt.Printf("  ✓ grupo %s → [%s]\n", g.Name, strings.Join(g.Destinations, ", "))
		}

		for _, r := range sp.RoutingRules {
			// los destinos referenciados deben existir en el namespace del proyecto
			if _, missingDests, err := st.ResolveDestinations(ctx, p.ID, r.Destinations); err != nil {
				return fmt.Errorf("regla %s: %w", r.Name, err)
			} else if len(missingDests) > 0 {
				return fmt.Errorf("regla %s: destinos desconocidos: %s", r.Name, strings.Join(missingDests, ", "))
			}
			enabled := true
			if r.Enabled != nil {
				enabled = *r.Enabled
			}
			priority := r.Priority
			if priority == 0 {
				priority = 100
			}
			rule := model.RoutingRule{
				Name: r.Name, MatchTags: r.MatchTags, MatchPriority: r.MatchPriority,
				MatchPayload: r.MatchPayload, Destinations: r.Destinations,
				StopProcessing: r.StopProcessing, Enabled: enabled, Priority: priority,
			}
			if _, err := st.UpsertRoutingRule(ctx, p.ID, rule); err != nil {
				return fmt.Errorf("regla %s: %w", r.Name, err)
			}
			fmt.Printf("  ✓ regla %s → [%s]\n", r.Name, strings.Join(r.Destinations, ", "))
		}
		// Reglas en DB ausentes del seed: se conservan (el import jamás
		// destruye config no declarada) pero se avisa para que no sorprendan.
		if existing, err := st.ListRoutingRules(ctx, p.ID); err != nil {
			return fmt.Errorf("listando reglas de %s: %w", sp.Name, err)
		} else {
			declared := map[string]bool{}
			for _, r := range sp.RoutingRules {
				declared[r.Name] = true
			}
			for _, r := range existing {
				if !declared[r.Name] {
					fmt.Printf("  · regla %s en DB no declarada en el seed: se conserva\n", r.Name)
				}
			}
		}

		for _, t := range sp.Templates {
			if err := st.UpsertTemplate(ctx, p.ID, t.Name, t.Body, nil); err != nil {
				return fmt.Errorf("plantilla %s: %w", t.Name, err)
			}
			fmt.Printf("  ✓ plantilla %s\n", t.Name)
		}
	}
	return nil
}

// upsertProject crea el proyecto si falta y, si ya existe, aplica las options
// declaradas. Sin options en el seed, no se toca nada.
func upsertProject(ctx context.Context, st *store.Store, sp seedProject) (store.Project, error) {
	declared := sp.Options.json()
	p, err := st.GetProjectByName(ctx, sp.Name)
	if errors.Is(err, store.ErrNotFound) {
		var opts json.RawMessage
		if len(declared) > 0 {
			opts = declared
		}
		p, err = st.CreateProject(ctx, sp.Name, opts)
		if err == nil {
			fmt.Printf("📁 proyecto %s creado (%s)\n", p.Name, p.ID)
		}
		return p, err
	}
	if err != nil {
		return store.Project{}, err
	}
	if len(declared) > 0 {
		changed, err := st.UpdateProjectOptions(ctx, p.ID, declared)
		if err != nil {
			return store.Project{}, fmt.Errorf("proyecto %s: options: %w", sp.Name, err)
		}
		if changed {
			fmt.Printf("  ↻ proyecto %s: options actualizadas\n", p.Name)
			// con las options en detail (paridad con el PATCH de la admin API):
			// el asiento permite reconstruir QUÉ se declaró, no solo que cambió
			st.InsertAudit(ctx, &p.ID, "seed", "project.options_updated", p.Name, declared)
		}
	}
	return p, nil
}
