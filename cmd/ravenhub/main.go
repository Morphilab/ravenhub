// Command ravenhub is the Hub service binary:
//
//	ravenhub serve     — API + workers (el servicio)
//	ravenhub migrate   — aplica migraciones y sale
//	ravenhub import    — aplica el seed YAML declarativo (infra-as-code)
//	ravenhub mcp       — servidor MCP (tool notify) para agentes, por stdio
//	ravenhub version   — versión
package main

import (
	"os"

	// tzdata embebido: la imagen alpine no trae /usr/share/zoneinfo y sin
	// esto las quiet hours con timezone fallan silenciosamente en contenedor
	_ "time/tzdata"

	"ravenhub/internal/cli"
	"ravenhub/internal/config"
)

func main() {
	config.LoadEnvFile(".env") // DX local: el compose ya inyecta env, esto cubre `go run`
	os.Exit(cli.Run(os.Args[1:]))
}
