# Spec — A2A (Agent2Agent) em nanobot-go

Status: **PLANEJADO / NÃO INICIADO**. Registrado em 2026-09-16 para não se
perder. Decisão do usuário: **servidor + cliente**, executado **depois** de
Memory/Dream + canais.

Fontes primárias consultadas:

- Especificação v1.0.0 (main): <https://github.com/a2aproject/A2A/blob/main/docs/specification.md>
- Especificação v0.3.0: <https://a2a-protocol.org/v0.3.0/specification/>
- TCK oficial: <https://github.com/a2aproject/a2a-tck>

Aviso de evidência: o documento v1.0.0 foi lido direto da fonte primária
(campos `supportedInterfaces`, `ROLE_USER`/`ROLE_AGENT`, `TASK_STATE_*`,
`protocolVersion: "1.0"`, `/.well-known/agent-card.json` confirmados). Os
**nomes PascalCase dos métodos JSON-RPC em v1.0.0** (`SendMessage` etc.) vêm de
fontes secundárias e **precisam ser reconfirmados na fonte primária** antes de
codificar. Não tratar como verificado.

---

## 1. Veredito de viabilidade

O binding **obrigatório** do A2A é JSON-RPC 2.0 sobre HTTP(S); gRPC e
HTTP+JSON/REST são **opcionais**. Isso significa que a conformidade obrigatória
cabe inteiramente na stdlib do Go — `net/http`, `encoding/json`, SSE via
`http.Flusher`. O zero-dependências do projeto é preservado.

**Fronteira dura:** o binding gRPC exige `google.golang.org/grpc` + protobuf e
**está fora de escopo**. Declaramos `supportedInterfaces` só com `JSONRPC`.

## 2. O que já existe no port (OBSERVED)

| Peça | Onde | Situação |
|---|---|---|
| Ponto de entrada programático | `internal/agent/loop.go:205` `ProcessMessage(ctx, InboundMessage) (*OutboundMessage, error)` | pronto |
| Cancelamento de turno | `internal/agent/loop.go:377` `cancelActive(key)` | existe, mas **não exportado** |
| Hook de progresso | `internal/agent/runner.go:80` `Hook` (`OnTextDelta`, `OnReasoningDelta`, `OnToolStart`, `OnToolEnd`) | definido |
| Streaming do provider | `internal/provider/provider.go:103` `StreamingProvider.ChatStream` | pronto |
| Leitor SSE | `internal/provider/openai/stream.go` (`sseReader`) | pronto — reusável no cliente A2A |
| Persistência atômica | `internal/session` (temp + `os.replace`, `.session-files.lock`) | padrão a reusar |
| Servidor HTTP | — | **não existe nenhum** |
| Camada de auth | — | **não existe nenhuma** |

## 3. Lacunas a fechar (OBSERVED, com file:line)

1. **`Loop` não plumba `Hook`.** `RunSpec` tem o campo (`runner.go:103`) mas
   `loop.go:250-268` monta o spec **sem** `Hook`. Sem isso não há delta
   streaming em `SendStreamingMessage`. ~20-30 LOC.
2. **`core.OutboundMessage` não tem `Event`** (`internal/core/types.go:78-86`)
   vs upstream `bus/events.py:68`. Mesmo bloqueio já reportado pelo
   reconhecimento de canais.
3. **`cancelActive` é não exportado** — `CancelTask` precisa de API pública.
4. **Nenhuma autenticação** — a spec exige que o servidor autentique toda
   requisição.

## 4. Mapeamento (o trabalho de engenharia real)

A2A é um protocolo de **tarefas**; o nanobot é um agente de **chat**. O
mapeamento é uma decisão de projeto, não um detalhe:

| A2A | nanobot |
|---|---|
| Task | um turno de `ProcessMessage` |
| `contextId` | session key (`<channel>:<chat_id>`) |
| `submitted` → `working` → `completed`/`failed`/`canceled` | aceitação → run → `StopReason` do runner |
| `input-required`, `auth-required` | **sem análogo** — o nanobot não pausa mid-turn para HITL. Declarar honestamente na capability; não fingir suporte. |
| Artifact (`TextPart`) | `res.FinalContent` |
| Artifact (`FileWithUri`) | arquivos do workspace |
| `CancelTask` | `Loop.Cancel(key)` (a exportar) |
| `SendStreamingMessage` | `Hook.OnTextDelta` → `TaskArtifactUpdateEvent` (append) |

## 5. Fronteira de segurança (decisão pendente do usuário)

Um nanobot exposto via A2A é **execução remota de código por design**: ele tem
tools de shell e de escrita em arquivo. A spec exige autenticar toda requisição,
mas autenticar ≠ autorizar. Decisão em aberto, **não tomar sozinho**:

- o endpoint A2A roda com o registry de tools completo, ou com um registry
  reduzido?
- auth por bearer/API-key estático é suficiente, ou é exigido JWT/OAuth2
  (verificável com `crypto/ecdsa`+`crypto/rsa`, mas manual)?

## 6. Fases propostas

1. **Núcleo do servidor**: `internal/a2a/` — envelope JSON-RPC 2.0, erros
   padrão e específicos, dispatch, Agent Card em `/.well-known/agent-card.json`.
2. **Task store**: persistência + paginação por cursor, reusando o padrão de
   escrita atômica do `internal/session`.
3. **Mapeamento**: `SendMessage` (bloqueante e `returnImmediately`),
   `GetTask`, `CancelTask`, `SubscribeToTask`.
4. **Streaming**: exportar `Cancel`, plumbar `Hook`, SSE writer.
5. **Push notifications**: `Create/Get/List/DeleteTaskPushNotificationConfig`
   + POST para webhook.
6. **Cliente**: tool `a2a_call` — descoberta de Agent Card, POST JSON-RPC,
   leitura SSE, mapeamento de `Task`/`Artifact` de volta para o modelo.
7. **Auth** (após a decisão da §5).
8. Opcional: binding HTTP+JSON/REST (mesma camada de dados, roteamento novo).

**INFERENCE (não medido):** servidor núcleo ≈ 1,5–2,5k LOC Go + testes;
cliente ≈ 400–700 LOC. Estimativa por analogia com o subsistema de memória —
não é medição.

## 7. Verificação

- **TCK oficial** (Python): `./run_tck.py --sut-url http://localhost:9999 --category mandatory`
  e `--category capabilities`. Exige instalar `a2a-tck` no `.tools/venv` (rede).
- **a2a-python SDK 1.x** como cliente real para teste de integração ponta-a-ponta
  (fala v1.0 e v0.3 em modo compat).
- Testes de unidade por comportamento, incluindo bordas adversariais: JSON-RPC
  malformado, `id` ausente/null, `params` ausente, método desconhecido
  (`-32601`), task em estado terminal recebendo mensagem, cancelar task já
  terminada, cliente SSE que desconecta no meio, webhook que devolve erro.
- Regra do projeto: **executar a referência antes de "corrigir" o port**.

## 8. Versão do protocolo

Recomendação: implementar **v1.0.0** como forma canônica (recupera `ListTasks`
no JSON-RPC, que em v0.3.0 era exclusivo de gRPC/REST) e **aceitar os nomes
v0.3** (`message/send`, `message/stream`, `tasks/get`, `tasks/cancel`) como
alias de entrada. Custo baixo, interoperabilidade bem maior. A confirmar com a
fonte primária na fase 1.
