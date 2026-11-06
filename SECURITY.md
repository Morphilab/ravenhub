# Seguridad — RavenHub

## Versiones soportadas

Solo la rama `main` y el último commit desplegado reciben correcciones. No hay
soporte de versiones antiguas ni de forks.

## Reportar una vulnerabilidad

- **No abras un issue público** con tokens, credenciales, datos personales ni
  pasos que permitan explotar el fallo.
- Usa el canal privado del operador del despliegue. Si el repositorio es
  público, usa **GitHub Security Advisories** (Security → Report a
  vulnerability) en lugar de un issue normal.
- Incluye: versión/commit, componente (Hub, SDK, MCP, scripts, compose),
  impacto, reproducción mínima, y si has visto datos reales afectados.
- Redacta secretos: `rvn_…`, `rva_admin_…`, bot tokens, contraseñas SMTP, emails
  y `chat_id`s.

## Alcance

Dentro:

- API HTTP del Hub (`/v1`, `/admin`, `/healthz`, `/readyz`, `/metrics`).
- Autenticación, scopes, aislamiento entre proyectos, cola/worker, cifrado,
  auditoría, retención y borrado.
- Adaptadores Telegram/email/webhook/Discord/Slack/`log`.
- SDK Python, servidor MCP, CLI, scripts de backup/restore/smoke, compose,
  Dockerfile y Caddyfile.
- Fugas de secretos o PII en logs, errores, backups o imágenes.

Fuera:

- Vulnerabilidades de Telegram, Gmail/SMTP, Discord, Slack o del host que no
  sean causadas por la configuración de RavenHub.
- Ingeniería social, acceso físico, o DoS con recursos ilimitados.
- Ataques que requieran un admin token ya comprometido (aunque el impacto de
  ese compromiso sí es de interés).

## Respuesta

- Acuse de recibo: mejor esfuerzo en 72 horas.
- Triage y gravedad: CRÍTICO/ALTO/MEDIO/BAJO/INFO.
- Coordinación de divulgación después del fix; no se promete un plazo fijo.
- No hay bug bounty ni compensación económica.

## Buenas prácticas para el operador

- Mantén `.env` en 600 y fuera de git; nunca pegues su contenido en issues.
- Rota tokens/credenciales ante cualquier sospecha.
- Ejecuta `make backup-verify` tras cada backup; sin identidad age el backup es
  irrecuperable.
- Revisa `govulncheck`, `staticcheck` y el escaneo de imagen antes de publicar
  y tras cambios de dependencias.
- No expongas el Hub de dev a Internet; el compose lo bindea a loopback.

## Historial

- 2026-10-19: documento creado.
- 2026-10-20: revisión documental.
- 2026-11-01: revisión de seguridad (crypto/auth/inyección/SSRF/privacidad/
  cola/deploy): Go 1.26.10 (GO-2026-6809), guardias de estado en la cola,
  expirador que respeta entregas vivas, cuota por proyecto con `send_at`
  alineado, canal `log` sin contenido por defecto, hardening dev=prod.
