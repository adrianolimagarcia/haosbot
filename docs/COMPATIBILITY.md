# Matriz de Compatibilidade — nanobot-go × nanobot (Python)

> **Documento de honestidade.** Esta matriz existe para impedir que o port
> "pareça funcionar" enquanto perde recursos silenciosamente. Toda linha
> marcada como implementada tem teste que a verifica. Toda linha não
> implementada está declarada explicitamente.

**Referência congelada:** `HKUDS/nanobot` @ `1bb712d3488915ca4ed9ccc1a93067ff722f5ab9`
(2026-09-16T02:38:34+08:00), versão `0.3.5`, 160.604 LOC Python.

---

## 1. Status por subsistema

Legenda: ✅ implementado e testado · 🟡 parcial · ❌ não implementado

| Subsistema | Python LOC | Status | Evidência / observação |
|---|---:|---|---|
| Vocabulário de tipos | — | ✅ | `internal/core` — 10 testes |
| `Message` round-trip | — | ✅ | Chaves desconhecidas preservadas em ordem |
| MessageBus | 718 | ✅ | `internal/bus` — 12 testes, race detector |
| Tool interface + Registry | — | ✅ | `internal/tools` — 17 testes |
| AgentRunner (loop LLM⇄tools) | 55 KB | 🟡 | `internal/agent` — núcleo fiel; ver §3 |
| AgentLoop (pipeline de turno) | 104 KB | 🟡 | `internal/agent` — 6 de 7 estágios |
| Context Builder / system prompt | 363 LOC | 🟡 | `internal/prompt` — 12 testes |
| Config engine | 1.356 | ✅ | `internal/config` — verificado diferencialmente contra o Python |
| Session store (JSONL) | 4.676 | ✅ | `internal/session` — storage_key verificado contra o Python |
| Provider OpenAI-compatible | 16.280 | 🟡 | `internal/provider/openai` — chat completions + SSE; Responses API não |
| Tools nativas (fs/shell) | — | 🟡 | `internal/tools/builtin` — 5 tools; web/grep/patch não |
| CLI | 9.051 | 🟡 | `run`, `chat`, `gateway`, `version`, `paths` |
| Gateway | 1.014 | ❌ | — |
| Canais (18) | 67.174 | ❌ | Maior componente do projeto |
| Memory + Dream | 51 KB | ❌ | — |
| Context compaction | 40 KB | ❌ | — |
| Subagents | 22 KB | ❌ | — |
| MCP (client) | — | ❌ | — |
| Plugins | 19 KB | ❌ | — |
| Cron / automações | 1.411 | ❌ | — |
| API OpenAI-compatible | 581 | ❌ | — |
| WebUI | 20.575 | ❌ | — |
| Skills | 754 | 🟡 | Apenas listagem no prompt |

**Total verificado nesta sessão:** 2.918 LOC Go de implementação, 2.833 LOC de
testes, 90 funções de teste, 11 benchmarks.

---

## 2. Correções ao plano inicial

Quatro afirmações do plano original estavam incorretas. Foram verificadas
diretamente no código:

| Afirmação original | Realidade verificada |
|---|---|
| Versão 0.3.0 | **0.3.5** (`pyproject.toml`) |
| Sessões em `~/.nanobot/sessions/<workspace-id>/*.jsonl` | `~/.nanobot/sessions/<workspace_id 32-hex>/<base64url_nopad(key)>.jsonl` — o subdiretório é um id hexadecimal de 32 chars, e o arquivo é o *session key* em base64url sem padding (`manager.py:1006`) |
| `concurrent_tools` é `False` por padrão | O dataclass `AgentRunSpec` tem default `False`, **mas `AgentLoop` passa `concurrent_tools=True` fixo** (`loop.py:1179`). Em produção, tools **executam em paralelo**. |
| `legacy_sessions_dir` é fallback de leitura | É **somente remoção**; nunca lido (`spec-session.md`) |
| `timezone` default é `"UTC"` | O campo é **declarado** `"UTC"`, mas o validator `resolve_timezone` o substitui por `detect_system_timezone()` sempre que `timezone_mode == "auto"` (o default). Medido: `AgentDefaults().timezone == "America/Sao_Paulo"`. Fornecer `timezone` sem `timezoneMode` implica `mode="manual"` e preserva o valor. |

### Verificação diferencial (executada)

As dependências do nanobot Python foram instaladas em `.tools/venv`, então a
referência **realmente executa** neste ambiente. Os valores abaixo foram
medidos, não inferidos:

```bash
.tools/venv/bin/python compat/python/dump_reference.py
```

| Valor | Medido no Python | Confere com o Go? |
|---|---|---|
| `max_tool_iterations` | 200 | ✅ |
| `max_tool_result_chars` | 16000 | ✅ |
| `temperature` | 0.1 | ✅ |
| `max_tokens` | 8192 | ✅ |
| `context_window_tokens` | 200000 | ✅ |
| `max_concurrent_subagents` | 4 | ✅ |
| `model` | `anthropic/claude-opus-4-5` | ✅ |
| `provider_retry_mode` | `standard` | ✅ |
| `tool_hint_max_length` | 40 | ✅ |
| `timezone` (efetivo) | `America/Sao_Paulo` | ⚠️ correção acima |
| Alias de `session_ttl_minutes` | aceita `sessionTtlMinutes`, `session_ttl_minutes`, `idleCompactAfterMinutes`; serializa como `idleCompactAfterMinutes` | 🔄 |
| `storage_key("cli:1")` | `Y2xpOjE` | 🔄 |
| Paths (`~/.nanobot/...`) | idênticos aos do CLI Go | ✅ |
| Classificação de retry (27 casos) | matriz completa | ✅ |
| Sanitização de histórico (12 fixtures) | reparo estrutural | ✅ |
| `InputBudget` (8 combinações) | aritmética do budget | ✅ |
| Normalização de tool result | marcadores, sanitização de nome, texto de referência | ✅ |
| Sessão escrita pelo Python → lida pelo Go | 6 mensagens, unicode, tool_calls | ✅ |
| Sessão escrita pelo Go → lida pelo Python | 4 mensagens, tool_call_id | ✅ |


---

## 3. Divergências conhecidas (deliberadas e documentadas)

Estas são diferenças conscientes, não bugs. Cada uma está comentada no código.

| # | Divergência | Motivo | Impacto |
|---|---|---|---|
| D1 | `runtime` no system prompt diz `Go go1.23.5`, não `Python 3.x` | Um binário Go reportar "Python" seria falso | Baixo — o modelo lê a linha como contexto |
| D2 | Jinja2 reimplementado em Go para 3 templates | Evitar dependência e superfície de ataque | Seções e ordem idênticas; whitespace byte-exato **não** é garantido |
| D3 | `NormalizeArguments` rejeita não-objetos diretamente | O Python preserva o valor e deixa o registry rejeitar; resultado observável é o mesmo (a chamada é recusada), mas o caminho difere | Nenhum no comportamento externo |
| D4 | Fila do bus ilimitada por padrão, com teto opcional | O Python usa `asyncio.Queue()` ilimitada. Ilimitado é risco de RAM em hardware pequeno, então há `MaxInbound`/`MaxOutbound` | Compatível por padrão; teto é opt-in |
| D5b | `exec` não usa `/tmp` como HOME do filho quando `HOME` não está definido | A referência é `os.environ.get("HOME", "/tmp")` (shell.py:792). Apontar o HOME de um processo filho para um diretório compartilhado, gravável por todos e (nesta máquina) residente em RAM é um risco desnecessário. Ordem: `HOME` → home do usuário → workspace → diretório atual | Caminho degenerado (HOME sempre definido na prática) |
| D5 | Paralelismo de tools sem limite numérico (igual ao Python) | Fiel à referência; `MaxParallelTools` é opt-in para hardware restrito | Fiel |

---

## 4. Comportamentos da referência **não** portados

Listados explicitamente para que a ausência não seja confundida com paridade.
Todos constam em comentário de pacote no código.

### AgentRunner
- Cadeias de recuperação de `length` (`_MAX_LENGTH_RECOVERIES = 3`)
- Callbacks de injeção (`_MAX_INJECTIONS_PER_TURN = 3`, `_MAX_INJECTION_CYCLES = 5`)
- Estado de conversa nativo do provider (`ProviderConversationState`)
- Compaction nativa (`provider_compaction_*`)
- Context governance / transcript builders
- Callbacks de checkpoint
- ~~Política de retry completa~~ **IMPLEMENTADO**: 4 tentativas com delays 1/2/4, `retry_after + 1` substituindo o cronograma, classificação de 429 (quota/billing permanente vs rate limit transitório), 402/408/409, fallback por marcadores de texto. Verificado diferencialmente contra o Python em 27 casos.

### AgentLoop
- Estágio COMPACT (auto-compaction e TTL por ociosidade)
- Runtime checkpoints e recuperação de interrupção pendente
- `SessionPolicy` (`disabled_tools`, `log_content`, `persist=false`)
- Staging de estado de conversa do provider
- Conjunto completo de comandos embutidos — apenas `/new`, `/help`, `/status`, `/stop`
- Segmentação de streaming e eventos de ciclo de vida do turno
- Hooks, subagents, turnos de cron/automação

### Persistência (descoberto no recon, alto risco)
A referência **descarta silenciosamente** ao persistir, e omitir isso **trava a sessão permanentemente**:
- mensagens assistant vazias são descartadas (`loop.py:2211-2212`)
- tool results com `tool_call_id` ausente, não declarado ou já atendido são descartados (`loop.py:2213-2227`)
- blocos `data:image/` são substituídos por placeholders (`loop.py:2090-2115`)

### Sanitização de request
`_enforce_role_alternation` (`base.py:1124-1197`) reescreve **todo** corpo de
request: mescla mensagens do mesmo papel, remove assistant à direita e injeta
`"(conversation continued)"`. `_sanitize_empty_content` (`base.py:814-873`)
reescreve conteúdo vazio e remove surrogates UTF-16 isolados. **Não implementado.**

---

## 5. Riscos de compatibilidade em aberto

Os quatro riscos marcados como **Altos** nesta tabela foram investigados um a
um. Três estavam resolvidos e a tabela estava desatualizada; **dois bugs reais**
foram encontrados no caminho.

| Risco | Estado | Evidência |
|---|---|---|
| `timestamp` naive local | ✅ Resolvido | `internal/session/timefmt.go`; e2e afirma ausência de `Z`/offset |
| Escaping JSON — `<`, `>`, `&` | ✅ Resolvido | `SetEscapeHTML(false)`; diferencial `TestStringEscapingMatchesPythonReference` |
| Escaping JSON — U+2028/U+2029 | 🔧 **Bug corrigido** | ver abaixo |
| Números float | 🔧 **Bug corrigido** | ver abaixo |
| Lock de arquivo de sessão | ✅ Resolvido | `internal/session/lock.go`, flock sobre `.session-files.lock` |
| Paridade de system prompt | Média, aberto | Whitespace Jinja2 não é byte-exato |
| Deriva do upstream | Média, inerente | `main` está ativo; o commit congelado é a especificação |

### Bug 1 — floats viravam inteiros ao salvar config

`Config.Save` usava `json.Encoder`, e o encoder do Go escreve a menor
representação que faz round-trip: `float64(1)` vira `1`. O Python escreve
`1.0` (`json.dumps`, loader.py:174).

Consequência: um `config.json` salvo por este port e lido pela referência
transforma o float em int. Pydantic aceita, então a falha é **silenciosa** — o
valor não é o que foi salvo.

Os limiares de notação científica também são do Python, não do Go: expoente
`15` fica decimal (`1000000000000000.0`) e `16` vira científico (`1e+16`); o
verbo `g` do Go troca em outro ponto.

### Bug 2 — U+2028/U+2029 escapados

`SetEscapeHTML(false)` resolve `<`, `>` e `&`, mas o Go escapa U+2028 e U+2029
**incondicionalmente**, por segurança de JSONP. O Python com
`ensure_ascii=False` os escreve literais. O bytes diferiam mesmo com os dois
documentos parseando para a mesma string.

### Por que isso importa e como foi corrigido

Ambos os formatadores vivem agora em `internal/pyjson`, compartilhado com o
session store. A duplicação anterior de uma lista de constantes entre pacotes
já tinha derivado uma vez; o formatador não é uma coisa que valha o risco.

O `UnescapeLineSeparators` conta a sequência de backslashes antes do escape e
pula quando ela é ímpar — um `bytes.Replace` ingênuo corromperia uma string que
contém literalmente os seis caracteres `\u2028`, porque o Go escapa a barra e
a sequência continua aparecendo como substring. Há teste para esse caso.

---

## 6. Verificação end-to-end (executada)

`scripts/e2e.sh` compila o binário real e o dirige contra um servidor
OpenAI-compatible simulado. É o único teste que exercita config → sessão →
provider HTTP → execução de tool → persistência através do binário compilado.

Resultado observado: **ALL PASS** nos 3 cenários (resposta simples, ida e volta
de tool, persistência de sessão).

### Bug encontrado e corrigido por este teste

O teste e2e encontrou um defeito real que os testes unitários não pegavam:

`core.Message` modela `tool_call_id` como **campo tipado**, mas
`toolMessage` (runner.go) gravava o valor via `SetExtra`, que escreve no
armazenamento de chaves desconhecidas. O `MarshalJSON` lê chaves modeladas dos
campos, então **toda mensagem de tool ia para o modelo com
`"tool_call_id": ""`**. Provedores OpenAI estritos rejeitam esse request, o que
teria travado permanentemente qualquer sessão com uso de tool.

Corrigido em dois níveis:
1. `toolMessage` agora atribui o campo tipado `ToolCallID`.
2. `Message.SetExtra` agora roteia chaves modeladas para seus campos, de modo
   que a classe inteira de bug não pode se repetir silenciosamente.

O teste de regressão é `internal/agent/toolcallid_test.go`.

## 7. Interoperabilidade de sessões (executada)

`compat/interop_test.go` prova as **duas direções** contra a implementação
Python real:

1. **Python escreve → Go lê.** Uma sessão com unicode (`acentuação, 日本語, 🎉`),
   `tool_calls`, `tool_call_id`, `name` e caracteres `<`, `>`, `&` é criada pelo
   `JsonlSessionStore` do Python e carregada pelo Go. Verificado: mesmo
   namespace de workspace, **mesmo caminho de arquivo**, conteúdo decodificado
   idêntico, ids de tool preservados, timestamps intactos.
2. **Go escreve → Python lê.** O inverso, incluindo a correlação
   `tool_call_id` e o unicode.

O namespace de workspace é a parte crítica: o `workspace-id` **não é derivado
do caminho** — é um id aleatório de 32 hex persistido em
`<workspace>/.nanobot/workspace-id`. Se o Go calculasse um id diferente, as
duas implementações manteriam históricos separados em silêncio. O teste
compara o diretório resolvido dos dois lados.

## 8. Governança de contexto (implementada)

`internal/agent/governance.go` porta `nanobot/agent/context_governance.py`. A
referência mantém um transcript cru em disco e constrói uma **cópia reparada**
para cada requisição, porque as APIs rejeitam a requisição inteira por defeitos
estruturais que se acumulam numa sessão longa:

| Defeito | Reparo |
|---|---|
| `tool` sem `tool_call_id` declarado, ou id duplicado | descartado |
| `tool_calls` do assistant sem resultado correspondente | resultado sintético inserido |
| `tool_call` com nome vazio | chamada removida (e seu resultado, por consequência) |
| placeholder de compaction replayado como turno real | removido |

Qualquer um deles **trava a sessão permanentemente**: toda requisição seguinte
falha, e a falha não é causada pelo turno novo. É a mesma classe do bug de
`tool_call_id` vazio que o harness e2e encontrou — aquele era uma instância,
isto é a guarda geral.

### Distinção que é fácil errar

A referência **repara sempre** e só **ajusta ao budget quando há pressão**.
`prepare_request` (context_governance.py:602) chama
`prepare_messages_for_model` antes de qualquer checagem de janela; a janela
condiciona apenas `fit_to_budget`. Um port que pulasse o reparo sem janela
configurada deixaria transcripts inválidos sem consertar — exatamente o caso em
que o provedor rejeita tudo.

### Estimativa de tokens

A referência usa `tiktoken` e cai para contagem de bytes UTF-8 quando ele não
está disponível (helpers.py:770). Este port tem zero dependências, então
implementa o fallback que a própria referência sanciona: 1 token por byte. Isso
**superestima** em ASCII e portanto falha para o lado seguro (compacta um pouco
antes do necessário). Contagens absolutas não coincidem com as do tiktoken; o
que é verificado diferencialmente é a decisão (mesmo transcript dentro ou fora
do budget).

### Divergência

A referência também funde mensagens `user` adjacentes
(`_merge_adjacent_user_messages_for_model`). Essa fusão depende da maquinaria de
marcadores de runtime-context, que este port não implementa. Consequência:
turnos consecutivos do usuário são enviados separados em vez de unidos.
Registrado como divergência.

## 9. Resultados de tool: normalização e offload

`internal/agent/toolresult.go` porta `normalize_tool_result`
(context_governance.py:709), `maybe_persist_tool_result` (helpers.py:580) e
`ensure_nonempty_tool_result` (runtime.py:48). Roda no **mesmo ponto** que a
referência: a construção da mensagem de tool (`runner.py:537`).

Dois problemas resolvidos:

1. **Resultado vazio é ambíguo.** Um modelo que recebe `""` não distingue "a
   tool rodou e não produziu nada" de "o resultado se perdeu", e tende a
   repetir a chamada em loop. O marcador
   `(nome completed with no output)` elimina a ambiguidade.
2. **Resultado gigante consome a janela.** A referência grava o payload em
   `<workspace>/.nanobot/tool-results/<sessão>/<call_id>.txt` e envia uma
   referência limitada, para que o modelo ainda possa ler a saída completa
   quando precisar. `read_file` é isento — ele tem seu próprio limite, e
   descartá-lo criaria um loop persist → read → persist.

### Divergência corrigida

O port tinha um `truncateToolResult` **inventado**, aplicado em `runOne`, com o
marcador `[... truncated: original result was N characters ...]`. Verifiquei que
`max_tool_result_chars` é usado na referência **apenas** dentro de
`normalize_tool_result` — não existe truncamento separado no runner. O
marcador inventado também não existe em lugar nenhum do upstream.

Removido. Consequência real: o truncamento acontecia **antes** de o conteúdo
poder ser persistido, então um resultado grande era destruído em vez de
guardado — o oposto do que a referência faz.

### Comportamento que parece bug mas não é

Com `max_tool_result_chars` pequeno, a referência colapsa o texto inteiro para
`[truncated: <caminho>]` (helpers.py:529), porque o preview sozinho tem 1200
chars e não caberia. Isso é correto e está coberto por teste — escrevi dois
testes que assumiam o contrário e ambos estavam errados, não o código.

## 10. Finalização por orçamento esgotado

Quando o limite de iterações é atingido, a referência **não desiste
imediatamente**: ela pede uma resposta final ao modelo **sem tools** e só cai
na mensagem estática se isso falhar (`_try_finalize_after_max_iterations`,
runner.py:1163; `finalize_on_max_iterations=True` por padrão, runner.py:112).

O port emitia a mensagem estática direto. A consequência era real: a conversa
naquele ponto normalmente já contém evidência suficiente para uma resposta útil,
e o trabalho do turno inteiro era descartado.

Implementado em `finalizeAfterMaxIterations`. As três condições de aceitação são
as da referência:

| Condição | Por quê |
|---|---|
| a requisição não falhou | um erro não é uma resposta |
| o modelo não pediu tools | pedir tools ignora o prompt, e executá-las contradiria o orçamento esgotado |
| o conteúdo não é em branco | uma resposta vazia não é melhor que o aviso estático |

A requisição de finalização é montada com `Tools: nil`, então os schemas não são
enviados **nem contados** pelo estimador de tokens — o budget é medido sobre o
que realmente vai na requisição.

Falha da própria tentativa não é erro: o chamador tem fallback, e transformar
uma tentativa de salvamento em falha dura seria pior que o aviso estático.

### Regressão que isso causou

`TestRunMaxIterations` afirmava 3 chamadas ao provedor. Agora são **4** (3
iterações + 1 finalização), que é o comportamento correto da referência. O teste
foi atualizado, não o código.

## 11. Recuperação de comprimento e tool calls aninhadas

### Recuperação de comprimento

Uma resposta cortada pelo limite de tokens de saída **não é uma resposta
completa**. A referência continua a resposta e junta os segmentos
(`_MAX_LENGTH_RECOVERIES = 3`, runner.py:72), citando de volta os últimos 64
caracteres já entregues para o modelo retomar do ponto exato
(`build_length_recovery_message`, runtime.py:78).

O port entregava a metade cortada como se fosse o resultado final.

Detalhes que o port acerta por construção:

- **Corte por runes, não bytes** — um corte no meio de um caractere multibyte
  enviaria UTF-8 inválido ao provedor.
- **A cadeia é quebrada por trabalho de tool** — depois de uma tool, o usuário
  não está mais lendo a mesma mensagem, então costurar produziria uma resposta
  sem sentido.

### Um comportamento que parece bug e não é

`_restore_outer_whitespace` **duplica** o whitespace de borda quando `content`
e `original` são a mesma string. Rodei a função do Python para conferir: com
`('The history of ', 'The history of ')` ela devolve `'The history of  '`.

A razão é que `finalize_content` do hook base é a identidade (hook.py:150), e
`clean` nunca foi limpo — então o whitespace é re-anexado a algo que ainda o
tinha. Reproduzir isso é o objetivo; dois testes meus assumiram o contrário e
estavam errados.

### Tool calls no formato aninhado

`core.ToolCall` não tinha `UnmarshalJSON`, então o formato da OpenAI
(`{"function":{"name":...,"arguments":...}}`) decodificava para uma chamada
**vazia, sem nome e sem argumentos** — silenciosamente, porque o
`encoding/json` ignora chaves desconhecidas.

O session compensava com `convertToolCalls`, mas qualquer outro chamador de
`Message.UnmarshalJSON` recebia uma chamada degenerada. Corrigido no tipo, com o
formato aninhado tendo precedência quando ambos aparecem — igual à referência.

O `core` **não** desembrulha arguments codificados como string: seu contrato é
passthrough fiel, e isso é responsabilidade do session, que agora chama
`core.NormalizeToolArguments`.

## 12. Como verificar

```bash
. scripts/goenv.sh                # toolchain pinado; GOTMPDIR fora do tmpfs
go build ./...
go vet ./...
go test -race ./internal/...
go test -bench=. -benchmem ./benchmarks/
```

Todos os comandos acima foram executados e passaram. Nenhum número de RAM é
publicado neste documento: **medições reais estão em `docs/BENCHMARKS.md`**, e
somente após serem medidas.

---

## 13. Critério para "drop-in compatible"

O projeto só será chamado de drop-in compatible quando **todos** os itens
abaixo estiverem em 100% com teste diferencial contra o Python:

```
Core · Providers · Tools · Memory · Skills · MCP · Subagents
Cron/automations · Channels · API · WebUI backend · Config
Workspace · Sessions
```

Hoje: **Core parcial**, Config/Sessions/Provider/Tools em implementação, o
resto não iniciado. **Este projeto não é, hoje, um substituto drop-in.**
