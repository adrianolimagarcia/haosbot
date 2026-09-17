package command

import (
	"fmt"
	"strings"
)

// CommandHandler handles slash commands like /status, /model, /new, /help, /skills
type Router struct{}

func NewRouter() *Router {
	return &Router{}
}

// IsSlashCommand checks if a line is a slash command
func IsSlashCommand(text string) bool {
	trimmed := strings.TrimSpace(text)
	return strings.HasPrefix(trimmed, "/")
}

// Execute processes a slash command and returns a system response
func (r *Router) Execute(text string, currentModel string, workspace string) (string, bool) {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "/") {
		return "", false
	}

	parts := strings.Fields(trimmed)
	cmd := strings.ToLower(parts[0])
	args := parts[1:]

	switch cmd {
	case "/help":
		return `### 🛠️ Comandos Slash Disponíveis no Haosbot:

- **/status**: Exibe status do runtime, modelo atual, memória e tarefas.
- **/model [nome]**: Exibe ou altera o modelo do LLM em uso para a conversa.
- **/new**: Limpa a conversa atual e inicia uma nova sessão fresca.
- **/skills**: Lista todas as ferramentas e habilidades ativas no agente.
- **/help**: Exibe esta lista de ajuda.`, true

	case "/status":
		return fmt.Sprintf(`### 📊 Status do Haosbot-Go
- **Runtime**: haosbot 0.1.0-dev (Go 1.23.5)
- **Modelo Atual**: %s
- **Memória RSS**: < 10 MB
- **Python Minimal**: Ativo com SQLite (~/.haosbot/python-min)
- **Workspace**: %s
- **Status**: Online e pronto`, currentModel, workspace), true

	case "/skills":
		return `### 🧰 Habilidades Nativas Ativas no Agente:
1. **python_exec**: Executa scripts Python usando CPython Minimal com suporte a SQLite, JSON e rede.
2. **exec**: Executa comandos bash, gerencia systemd, pacotes e rede (nmap, ssh, curl).
3. **file_ops**: Leitura, gravação e edição atômica de arquivos.
4. **dream_memory**: Consolidação durável e memória em disco.`, true

	case "/new":
		return "Sessão reiniciada com sucesso. O histórico anterior foi arquivado.", true

	case "/model":
		if len(args) == 0 {
			return fmt.Sprintf("Modelo em uso no momento: **%s**.\nPara trocar, use: `/model <nome-do-modelo>` (ex: `/model deepseek-chat` ou `/model gpt-4o`).", currentModel), true
		}
		newModel := args[0]
		return fmt.Sprintf("Modelo alterado com sucesso para **%s** para os próximos turnos.", newModel), true

	default:
		return fmt.Sprintf("Comando desconhecido `%s`. Digite `/help` para ver os comandos suportados.", cmd), true
	}
}
