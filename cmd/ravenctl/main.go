// Command ravenctl es el CLI de administración de RavenHub:
// la misma base de código que `ravenhub`, con `import` como acción por
// defecto para rutas sueltas.
//
//	ravenctl dev/seed.yaml   — aplica el seed (infra-as-code, idempotente)
//	ravenctl migrate         — aplica migraciones
package main

import (
	"fmt"
	"os"

	_ "time/tzdata"

	"ravenhub/internal/cli"
	"ravenhub/internal/config"
)

func main() {
	config.LoadEnvFile(".env")
	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "uso: ravenctl <seed.yaml> | migrate | version")
		os.Exit(2)
	}
	switch args[0] {
	case "migrate", "version", "import":
		// subcomando explícito
	default:
		args = append([]string{"import"}, args...) // ruta suelta → import
	}
	os.Exit(cli.Run(args))
}
