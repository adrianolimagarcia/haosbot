# Manual Completo do Haosbot-Go

O **haosbot-go** é uma reescrita completa e autônoma do framework haosbot em **Go** puro, concebida para fornecer alta performance, baixo consumo de memória (~5–10 MB RSS), zero dependências externas e total compatibilidade com o formato de dados em disco (`~/.haosbot/`).

---

## Índice
1. [Conteúdo do Pacote](#1-conteúdo-do-pacote)
2. [Instalação Standalone e Systemd](#2-instalação-standalone-e-systemd)
3. [Configuração do Agente e LLMs Externos](#3-configuração-do-agente-e-llms-externos)
4. [Autenticação e Segurança (Bearer Token)](#4-autenticação-e-segurança-bearer-token)
5. [Servidor HTTP / API OpenAI-Compatible](#5-servidor-http--api-openai-compatible)
6. [Integração com a WebUI](#6-integração-com-a-webui)
7. [Ferramentas Nativas, Shell e Python Exec](#7-ferramentas-nativas-shell-e-python-exec)
8. [CPython Minimal com SQLite](#8-cpython-minimal-com-sqlite)
9. [Canais de Mensagens (Telegram)](#9-canais-de-mensagens-telegram)
10. [Referência Completa de Comandos CLI](#10-referência-completa-de-comandos-cli)

---

## 1. Conteúdo do Pacote

O arquivo `haosbot-standalone.tar.gz` contém:
- **`haosbot`**: O executável único compilado (~12 MB).
- **`install.sh`**: Script de instalação automatizada para Linux.
- **`scripts/build_cpython_min.sh`**: Automação para compilar o CPython Minimal enxuto (~10–20 MB) com suporte a SQLite.
- **`README.md`**: Guia rápido de introdução.
- **`MANUAL.md`**: Este manual de referência completo.
- **`cmd/` & `internal/`**: Todo o código-fonte em Go do projeto.

---

## 2. Instalação Standalone e Systemd

Para instalar o haosbot-go no seu sistema:

```bash
# 1. Extrair o arquivo
tar -xzf haosbot-standalone.tar.gz
cd haosbot

# 2. Executar o instalador
./install.sh
```

### O que o instalador faz:
- Copia o binário para `~/.local/bin/haosbot`.
- Cria a estrutura completa de diretórios em `~/.haosbot/`:
  - `workspace/`: Arquivos de perfil do agente (`SOUL.md`, `USER.md`, `AGENTS.md`), histórico de memória e skills.
  - `sessions/`: Transcrições de conversas por canal e sessão em formato JSONL.
  - `logs/`: Logs de saída e de erro do gateway.
  - `media/`: Arquivos multimídia recebidos por canais.
- Cria o serviço de usuário systemd em `~/.config/systemd/user/haosbot.service`.

### Comandos de Gerenciamento do Serviço (Systemd):
```bash
# Recarregar as configurações de serviços do usuário
systemctl --user daemon-reload

# Habilitar para iniciar no boot do usuário e iniciar agora
systemctl --user enable --now haosbot

# Verificar status de execução
systemctl --user status haosbot

# Parar o serviço
systemctl --user stop haosbot

# Ver logs em tempo real
tail -f ~/.haosbot/logs/gateway.log
```

---

## 3. Configuração do Agente e LLMs Externos

Toda a configuração reside em `~/.haosbot/config.json`. O `haosbot-go` suporta qualquer provedor externo compatível com OpenAI, DeepSeek, Anthropic, Ollama, vLLM ou LiteLLM.

Exemplo de configuração completa em `~/.haosbot/config.json`:

```json
{
  "agents": {
    "defaults": {
      "model": "deepseek-chat",
      "temperature": 0.7,
      "maxTokens": 4096
    }
  },
  "providers": {
    "openai": {
      "apiKey": "sk-sua-chave-aqui",
      "baseUrl": "https://api.deepseek.com/v1"
    }
  },
  "api": {
    "host": "127.0.0.1",
    "port": 8765,
    "apiKey": "seu-token-secreto-para-proteger-o-haosbot"
  }
}
```

Para usar **Ollama** local (sem gastar com APIs externas):
```json
"providers": {
  "openai": {
    "apiKey": "ollama",
    "baseUrl": "http://127.0.0.1:11434/v1"
  }
}
```

---

## 4. Autenticação e Segurança (Bearer Token)

No objeto `"api"` do `config.json`, configure a chave `"apiKey"`:
- **Se `apiKey` estiver vazia (`""`)**: O servidor HTTP do haosbot aceita conexões locais livremente.
- **Se `apiKey` estiver preenchida**: Todas as chamadas para rotas da API (como `/v1/models` e `/v1/chat/completions`) exigem o header HTTP:
  ```http
  Authorization: Bearer <seu-token>
  ```
- O endpoint `/health` permanece aberto sem autenticação para monitoramento de liveness.

---

## 5. Servidor HTTP / API OpenAI-Compatible

O gateway inicia um servidor HTTP nativo com os seguintes endpoints:

- **`GET /health`**: Retorna `{"status":"ok","runtime":"haosbot-go"}`.
- **`GET /v1/models`**: Lista os modelos configurados.
- **`POST /v1/chat/completions`**: Recebe requisições no formato padrão OpenAI e despacha o turno para o loop do agente e provedor configurado.

Para iniciar o gateway em uma porta customizada:
```bash
haosbot gateway --port 8900
```

---

## 5. Servidor HTTP / API OpenAI-Compatible

O gateway inicia um servidor HTTP nativo com os seguintes endpoints:

- **`GET /`**: Serve a WebUI embutida (Control Center) — abre `http://IP:8765/` no navegador.
- **`GET /health`**: Retorna `{"status":"ok","runtime":"haosbot-go"}`.
- **`GET /v1/models`**: Lista os modelos configurados.
- **`POST /v1/chat/completions`**: Recebe requisições no formato padrão OpenAI e despacha o turno para o loop do agente e provedor configurado.
- **`GET /api/config`**: Lê a configuração atual (`~/.haosbot/config.json`) — usado pelo painel visual.
- **`POST /api/config`**: Salva a configuração no servidor e recarrega o provedor LLM em tempo real — **sem precisar editar o arquivo manualmente**.

### Comandos Slash (funcionam no chat da WebUI, no CLI e via API)

| Comando | Função |
|---|---|
| `/help` | Lista todos os comandos disponíveis |
| `/status` | Exibe status do runtime, modelo atual, memória e workspace |
| `/skills` | Lista as habilidades/ferramentas ativas (python_exec, exec, file_ops, dream_memory) |
| `/model [nome]` | Mostra ou troca o modelo em uso (ex: `/model deepseek-chat`) |
| `/new` | Limpa a conversa atual e inicia uma nova sessão |

### Painel Visual de Configuração (WebUI)

Na WebUI, clique em **"Configuração Visual"** (canto inferior esquerdo) para alterar sem tocar em arquivos:

1. **Provedor LLM Externo (API)**: URL base (ex: `https://api.deepseek.com/v1` ou `http://localhost:11434/v1`), chave de API e modelo padrão.
2. **Segurança do Gateway**: Bearer Token que protege todas as rotas e a WebUI.
3. Clique em **"Salvar no Servidor"** — a configuração é gravada em `~/.haosbot/config.json` e o provedor é recarregado imediatamente.

Para iniciar o gateway em uma porta customizada:
```bash
haosbot gateway --port 8900
```

Para expor na rede (ex: IP Tailscale):
```bash
haosbot gateway --host 100.76.224.27 --port 8765
```
## 6. Integração com a WebUI

Para conectar uma interface gráfica (como a WebUI React/TypeScript do haosbot):
1. Configure o cliente da WebUI para se conectar à URL do gateway (ex: `http://localhost:8765`).
2. Se configurou `api.apiKey`, insira o mesmo token na tela de configuração da WebUI.
3. A WebUI enviará as mensagens diretamente para `/v1/chat/completions` e receberá as respostas processadas.

---

## 7. Ferramentas Nativas, Shell e Python Exec

O agente tem acesso a ferramentas embutidas de alta segurança e controle:
- **`exec`**: Execução controlada de comandos de shell com timeout, limite de saída de caracteres e restrição de diretório.
- **`read_file`**, **`write_file`**, **`edit_file`**, **`apply_patch`**: Manipulação determinística de arquivos no workspace.
- **`python_exec`**: Execução de scripts e trechos Python dinâmicos (`python3 -c "..."`).

---

## 8. CPython Minimal com SQLite

Para ambientes sem Python ou para manter um interpretador ultra-enxuto com SQLite:
1. Execute o script automatizado:
   ```bash
   ./scripts/build_cpython_min.sh
   ```
2. O script compilará um CPython otimizado em `~/.haosbot/python-min/` removendo testes, tkinter e módulos pesados, mas **mantendo explicitamente o `sqlite3`**, `asyncio`, `socket` e `json`.
3. A ferramenta `python_exec` detecta automaticamente esse runtime e passa a utilizá-lo prioritariamente.

---

## 9. Canais de Mensagens (Telegram)

O haosbot possui suporte nativo ao **Telegram Bot**:
No `~/.haosbot/config.json`, configure a seção do Telegram:
```json
{
  "channels": {
    "telegram": {
      "enabled": true,
      "botToken": "SEU_TELEGRAM_BOT_TOKEN",
      "allowedUsers": ["seu_usuario_telegram"]
    }
  }
}
```
Ao iniciar `haosbot gateway`, o canal do Telegram fará polling de mensagens e responderá diretamente pelo chat.

---

## 10. Referência Completa de Comandos CLI

- `haosbot version`: Mostra a versão do haosbot-go, versão do compilador Go e commit de compatibilidade.
- `haosbot paths`: Exibe o mapeamento e o status de existência dos diretórios em `~/.haosbot/`.
- `haosbot gateway [--port 8765]`: Inicia o runtime de serviço, o servidor HTTP e os canais em background.
- `haosbot chat`: Inicia uma sessão de chat interativa via terminal.
- `haosbot run "<mensagem>"`: Executa um turno único e imprime a resposta no terminal.


## 11. Memória Híbrida MicroGraphRAG

O haosbot integra o [MicroGraphRAG-Go](https://github.com/adrianolimagarcia/micrographrag-go) como índice derivado local em `~/.haosbot/agent.db` (ou no diretório legado ativo). A persistência autoritativa continua sendo o transcript de sessões e `workspace/memory/MEMORY.md`.

- SQLite + FTS5 + grafo são habilitados por padrão.
- Cada turno concluído é indexado de forma assíncrona; a resposta não espera o índice.
- Antes de cada turno, a busca híbrida recupera até 6 trechos e injeta no prompt como **dados não confiáveis**, sem persistir o bloco renderizado no transcript.
- Falhas do índice são fail-soft: o agente continua funcionando com o contexto normal.
- Embeddings locais/vector ficam desligados por padrão para manter baixo consumo. Para habilitá-los, forneça o runtime/modelo Potion local e faça build CGO com Go 1.25+.
- Diagnóstico: `agent.db` é um índice derivado; não substitui `MEMORY.md` nem os JSONL de sessão.

A dependência está fixada no commit `fdb8fd561f1c2fec039a201f0eda1551735ff806`; o build integrado requer Go 1.25+ e CGO. O runtime padrão usa FTS5/grafo sem embedder Potion para evitar download durante operação.

## 12. Protocolo Agent2Agent (A2A)

O **haosbot-go** implementa a especificação oficial do protocolo [Agent2Agent (A2A)](https://github.com/a2aproject/A2A) em Go puro, sem dependências e mantendo o consumo de memória em repouso (< 10 MB RSS).

### A. Discovery de Agente (`GET /.well-known/agent-card.json`)
Qualquer agente ou orquestrador na rede pode descobrir o haosbot consultando:
```bash
curl http://100.76.224.27:8765/.well-known/agent-card.json
```
Retorna metadados do agente, URL de transporte e as skills anunciadas (`exec`, `python_exec`, `file_ops`).

### B. Execução de Tarefas A2A (`POST /a2a` via JSON-RPC 2.0)
Agentes externos podem delegar tarefas para o haosbot usando JSON-RPC 2.0 padrão:
```bash
curl -X POST http://100.76.224.27:8765/a2a \
  -H "Content-Type: application/json" \
  -d '{
    "jsonrpc": "2.0",
    "id": 1,
    "method": "tasks/send",
    "params": {
      "message": { "text": "verifique o uso de disco e memória da máquina" }
    }
  }'
```
A requisição é despachada diretamente para o `agent.Loop` autônomo com ferramentas completas, e a resposta é devolvida no formato A2A com `status: "completed"`.

### C. Ferramenta de Saída: `a2a_call`
O próprio haosbot possui a ferramenta nativa `a2a_call` para descobrir outros agentes (`action: "discover"`) e delegar sub-tarefas para agentes remotos (`action: "send"`).
