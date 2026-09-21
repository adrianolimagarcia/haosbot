# Spec — A2A (Agent2Agent) em nanobot-go

Status: **IMPLEMENTADO (binding JSON-RPC, protocolo v1.0)**. Servidor em
`internal/a2a/`, cliente na tool `a2a_call` (`internal/tools/builtin/a2a_call.go`).
Registrado em 2026-09-16 como planejamento; implementado e verificado contra as
ferramentas oficiais em 2026-09-22.

Fontes primárias consultadas:

- Especificação v1.0.0: <https://github.com/a2aproject/A2A/blob/main/docs/specification.md>
- Proto oficial: `specification/a2a.proto` no mesmo repositório
- JSON Schema oficial: <https://a2a-protocol.org/v1.0.0/spec/a2a.json>
- TCK oficial: <https://github.com/a2aproject/a2a-tck> (@ `263b9cf`)
- SDK oficial: <https://github.com/a2aproject/a2a-python> (`a2a-sdk` 1.1.5)

**Evidência.** Os nomes PascalCase dos métodos JSON-RPC em v1.0.0
(`SendMessage`, `GetTask`, `ListTasks`, `CancelTask`, `SendStreamingMessage`,
`SubscribeToTask`, `Create/Get/List/DeleteTaskPushNotificationConfig`,
`GetExtendedAgentCard`) estão **confirmados na fonte primária** (linha 2247 da
spec) e no dispatch do SDK oficial
(`a2a/server/routes/jsonrpc_dispatcher.py`). O aviso de "não verificado" da
versão anterior deste documento está resolvido.

---

## 1. Veredito de viabilidade

O binding **obrigatório** do A2A é JSON-RPC 2.0 sobre HTTP(S); gRPC e
HTTP+JSON/REST são **opcionais**. A conformidade obrigatória cabe inteiramente na
stdlib do Go — `net/http`, `encoding/json`, SSE via `http.Flusher`. O
zero-dependências do projeto foi preservado.

**Fronteira dura:** o binding gRPC exige `google.golang.org/grpc` + protobuf e
**está fora de escopo**. A card declara `supportedInterfaces` só com `JSONRPC`.

## 2. O que foi implementado

### Servidor — `internal/a2a/`

| Peça | Onde | Situação |
|---|---|---|
| Modelo de dados v1.0 | `types.go` | `AgentCard`, `AgentInterface`, `Part` (oneof sem `kind`), `Message`, `Task`, `TaskStatus`, `Artifact`, `ListTasksResponse` |
| Agent Card | `handler.go` `agentCard()` | `supportedInterfaces`, `defaultInputModes`/`defaultOutputModes`, `skills[].tags`; `securitySchemes`/`security` quando há API key |
| Cache da card | `handleAgentCard` | `Cache-Control`, `ETag`, `Last-Modified`; `If-None-Match` e `If-Modified-Since` → 304 |
| Dispatch JSON-RPC | `handleJSONRPC` | nomes PascalCase v1.0 + aliases pré-1.0 |
| `SendMessage` | `handleSendMessage` | resposta `SendMessageResponse{task\|message}`, `artifacts` + `history` |
| `GetTask` / `ListTasks` / `CancelTask` | handlers dedicados | `historyLength`, paginação por cursor, cancelamento idempotente |
| Streaming (SSE) | `stream.go` | `SendStreamingMessage` e `SubscribeToTask` em `text/event-stream`; fan-out por task, ordem preservada |
| Códigos de erro reservados | `types.go` | `-32001`..`-32009` conforme a spec |
| `A2A-Version` | `handleJSONRPC` | versão não servida → `-32009` |

Métodos v1.0 não suportados respondem o erro **específico da capability**, não
`-32601`: push notifications → `-32003` `PushNotificationNotSupported`,
`GetExtendedAgentCard` → `-32004` `UnsupportedOperation`. A card declara
`streaming: true`, `pushNotifications: false`, `extendedAgentCard: false`.

O streaming usa SSE: cada linha `data:` é uma resposta JSON-RPC embrulhando um
`StreamResponse`. Um turno não expõe deltas intermediários — ele roda até o fim e
publica o resultado — então o stream carrega os estados que a task realmente
atravessa: `WORKING` na aceitação e o estado terminal no fim. O `taskHub`
(`stream.go`) faz o fan-out dos snapshots publicados, então **todas** as streams
ativas de uma task recebem os mesmos eventos na mesma ordem, e fechar uma não
afeta as outras. `SubscribeToTask` emite o estado atual como primeiro evento e
recusa task terminal com `-32004`.

O endpoint responde em `/a2a` **e** `/a2a/`. A barra final não é exigida pela
spec, mas é o que clientes reais produzem: o TCK oficial monta o cliente HTTP com
a URL da interface como `base_url`, e o httpx normaliza isso para `<url>/`. O
`protectedPath` de `internal/api/security.go` cobre as duas formas — sem isso a
rota extra ficaria acessível sem bearer token.

### Cliente — `a2a_call`

- `discover`: lê a card, reporta `supportedInterfaces` e skills; aceita card v0.3
  (campo `url`).
- `send`: `SendMessage` com `messageId`, `role` e `parts`; envia
  `A2A-Version: 1.0`; posta na URL anunciada pela card (fallback `<base>/a2a`).
- Compat v0.3: se o par responde `-32601`, refaz uma vez com `message/send` e o
  discriminador `kind`.

## 3. Mapeamento (decisão de projeto)

A2A é um protocolo de **tarefas**; o nanobot é um agente de **chat**.

| A2A | nanobot |
|---|---|
| Task | um turno de `ProcessMessage` |
| `contextId` | contexto da task, inferido da task quando a mensagem só traz `taskId` |
| `TASK_STATE_WORKING` → `COMPLETED`/`FAILED`/`CANCELED` | aceitação → run → resultado |
| `TASK_STATE_INPUT_REQUIRED`, `AUTH_REQUIRED` | **sem análogo** — o nanobot não pausa mid-turn para HITL. Não são emitidos. |
| Artifact (`TextPart`) | texto final do turno |
| `CancelTask` | cancelamento real do turno em execução (`Handler.cancels`) |

## 4. Fronteira de segurança

Um nanobot exposto via A2A é **execução remota de código por design**: ele tem
tools de shell e de escrita em arquivo. O que existe hoje:

- com `api.apiKey` configurado, toda requisição a `/a2a` (e `/a2a/`) exige
  `Authorization: Bearer <key>`; a card anuncia o esquema em `securitySchemes`;
- sem `api.apiKey`, o servidor só aceita peer de loopback (`validateBindAddr`).

**Decisão ainda em aberto (do usuário), não tomada sozinho:** o endpoint A2A roda
com o registry de tools completo, ou com um registry reduzido? Bearer/API-key
estático é suficiente, ou é exigido JWT/OAuth2?

## 5. O que NÃO está implementado

- **Push notifications** — declarado `false`, responde `-32003`.
- **Extended Agent Card** — declarado `false`, responde `-32004`.
- **Binding gRPC** e **HTTP+JSON/REST** — fora de escopo; não anunciados.
- **Assinatura da card (JWS)** e **OAuth2/OIDC** — não implementados.

## 6. Limitação conhecida

Uma falha do provider LLM chega ao A2A como **texto**, não como erro: a camada de
provider (`internal/provider/openai/stream.go`) converte a falha em
`content: "Error calling LLM: ..."` com `FinishReason: FinishError`, e o
`core.OutboundMessage` que o loop devolve não carrega esse finish reason. A task
é portanto publicada como `TASK_STATE_COMPLETED` com o texto do erro como
artifact. Um turno que devolve erro pelo caminho normal do loop já vira
`TASK_STATE_FAILED`; o caso acima é o do provider que engole o erro em conteúdo.
Corrigir isso é mudar a semântica de erro do provider para todos os canais, não
um ajuste local do A2A — por isso não foi feito aqui.

## 7. Verificação (executada)

- **TCK oficial** `a2aproject/a2a-tck` @ `263b9cf`, binding JSON-RPC,
  `--transport jsonrpc`:
  - antes: 4.7% overall, 5.3% must, 225 erros (a fixture aborta em
    "Agent card declares no supportedInterfaces");
  - depois: **68.1% overall, 69.1% must, 42.9% should, 100% may, 0 erros**, com
    **3 requisitos MUST reprovando** (56 MUST aprovando, 33 pulados, 22 não
    automatáveis).
- Os 3 MUST restantes **não são defeitos de protocolo**:
  - `DM-ART-001` (4 testes) e `DM-MSG-001` conferem os payloads fixos do agente
    de referência que o próprio TCK embarca (`sut/a2a-python/sut_agent.py`
    ramifica por prefixo de `messageId` e devolve "Generated text content",
    "Direct message response", etc.). Um agente de propósito geral que roteia
    para um LLM real não produz essas strings.
  - `CORE-SEND-003` declara `expected_error=None` embora o texto do requisito
    exija `ContentTypeNotSupportedError`; o harness
    (`tests/compatibility/core_operations/test_requirements.py`) então exige
    **sucesso**. O agente de referência "passa" devolvendo sucesso e violando o
    próprio MUST. Nós devolvemos `-32005`, que é o que o requisito pede.
- Para comparação, o agente de referência do próprio TCK, no mesmo binding,
  reprova 1 MUST (`STREAM-SUB-003`) e 5 requisitos no total, e é pior em SHOULD
  (22,2% contra 42,9%) e MAY (75% contra 100%).
- **SDK oficial `a2a-sdk` 1.1.5**: resolve a card, desserializa a `Task` ponta a
  ponta e consome o stream SSE (`streaming=True`, 2 eventos: `WORKING` e
  `COMPLETED`); antes falhava com `contextId Field required` e `status Input
  should be a valid dictionary`.
- **Cliente oficial → nosso servidor** e **nosso cliente → agente de referência**
  verificados nos dois sentidos.
- Suíte local: `go test ./...` verde, `-race -count=3 -shuffle=on` em
  `internal/a2a` e `internal/api`, `go vet`, `staticcheck`, build com
  `-tags sqlite_fts5`.

## 8. Versão do protocolo

Implementado **v1.0** como forma canônica. Os nomes pré-1.0 (`tasks/send`,
`message/send`, `tasks/create`, `tasks/get`) continuam aceitos como **alias de
entrada**, o que a spec permite durante o período de sobreposição; a card
anuncia apenas `protocolVersion: "1.0"` e `A2A-Version` aceita `1.0` e `0.3`.
