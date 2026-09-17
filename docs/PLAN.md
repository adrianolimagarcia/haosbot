# nanobot-go — Plano de Implementação

> Reimplementação compatível do nanobot em Go. Não é uma tradução mecânica
> Python→Go: é um runtime alternativo que mantém comportamento, configuração e
> dados do nanobot original.

---

## 1. Fatos verificados (recon)

Todos os números abaixo foram medidos diretamente no upstream congelado, não
estimados.

| Fato | Valor | Como foi verificado |
|---|---|---|
| Upstream | `HKUDS/nanobot` @ `1bb712d3488915ca4ed9ccc1a93067ff722f5ab9` | `git rev-parse HEAD` |
| Data do commit | 2026-09-16T02:38:34+08:00 | `git log -1 --format=%cI` |
| `adrianolimagarcia/nanobot` | **mesmo HEAD** que `HKUDS/nanobot` | `git ls-remote` nos dois |
| Versão do pacote | **0.3.5** (não 0.3.0) | `pyproject.toml` |
| Total Python | **160.604 LOC** / 399 arquivos | `find -name '*.py' \| xargs cat \| wc -l` |
| Maior componente | `channels/` — **67.174 LOC**, 149 arquivos, ~18 canais | idem, por diretório |
| Core do agente | `agent/` — 23.186 LOC | idem |
| Providers | `providers/` — 16.280 LOC (`base.py` sozinho: 1.922 LOC) | idem |
| WebUI | `webui/` — 20.575 LOC | idem |
| Sessões | `session/` — 4.676 LOC | idem |
| Arquivos críticos | `agent/loop.py` 104 KB, `agent/runner.py` 56 KB, `agent/memory.py` 52 KB, `agent/context_governance.py` 40 KB | `ls -la` |

### Correções ao plano inicial

O plano original continha quatro imprecisões que a leitura do código corrigiu:

1. **Versão**: o projeto está em **0.3.5**, não 0.3.0.
2. **Layout de sessões**: sessões ficam em `~/.nanobot/sessions/<base64url(key)>.jsonl`,
   **não** em `~/.nanobot/sessions/<workspace-id>/*.jsonl`. O nome do arquivo é o
   session key codificado em base64url com padding removido
   (`JsonlSessionStore.storage_key`, `session/manager.py:1006`).
3. **`AgentLoop` × `AgentRunner`**: a separação existe e está confirmada
   (`loop.py:196`, `runner.py:141`), mas `loop.py` tem 104 KB — o AgentLoop é
   muito maior que "turno/canal/sessão" sugere. Ele contém dispatch de comandos,
   resolução de runtime, presets de modelo, turnos de cron/automação e delivery.
4. **`~/.nanobot/` layout**: confirmado, mais subdiretórios não citados:
   `cron/`, `logs/`, `media/`, `webui/`, `history/cli_history`, e um
   `sessions/` legado usado como fallback de migração (`config/paths.py`).

---

## 2. Objetivo e escopo

### Objetivo

Runtime alternativo do nanobot em Go: binário único, sem Python/pip/venv/uv/Node
em runtime, consumindo o mesmo `~/.nanobot/`.

### Realidade de escopo

**160.604 LOC de Python não são portáveis em uma sessão.** Uma paridade de 100%
com os 18 canais é um projeto de vários meses para um time. Qualquer afirmação
em contrário seria desonesta.

Este documento separa explicitamente:

- **ENTREGUE NESTA SESSÃO** — o núcleo (spine), verificado por testes.
- **ROADMAP** — o caminho até paridade, em milestones.

### O que esta sessão entrega

Um `nanobot-go` que realmente funciona ponta a ponta:

```
~/.nanobot/config.json  (formato real do Python)
        │
        ▼
   config engine  ──►  CLI  ──►  MessageBus  ──►  AgentLoop
                                                      │
                                                      ▼
                                                 AgentRunner
                                                  │       │
                                        Provider ◄─┘       └─► ToolRegistry
                                     (OpenAI-compat)          (filesystem,
                                       + streaming             shell, web)
                                                      │
                                                      ▼
                              ~/.nanobot/sessions/<base64url>.jsonl  (formato real)
```

---

## 3. Arquitetura alvo

```
                    nanobot-go
                        │
               ┌────────▼────────┐
               │     Gateway     │  (daemon: canais + cron + HTTP)
               └────────┬────────┘
                        │
                  MessageBus  (channels Go, backpressure explícito)
                        │
                ┌───────▼───────┐
                │   AgentLoop   │  turno / sessão / comando / delivery
                └───────┬───────┘
                        │
                Context Builder
                        │
                ┌───────▼───────┐
                │  AgentRunner  │  loop LLM ⇄ tools
                └───────┬───────┘
                        │
                 Provider Router
                        │
                      LLM
               reasoning / tools
                        │
                  ToolRegistry
                        │
        ┌───────────────┼───────────────┐
        ▼               ▼               ▼
      native           MCP          subagents
      tools
```

### Decisões de engenharia

| Decisão | Motivo |
|---|---|
| **Zero dependências externas** (stdlib apenas) | Objetivo é binário pequeno. `golang.org/x/sync@latest` força upgrade para Go 1.26; `errgroup` é substituível por `sync.WaitGroup` + semáforo. Sem supply chain, sem surpresa de toolchain. |
| **Toolchain pinado** (`GOTOOLCHAIN=local`, Go 1.23.5) | Builds reprodutíveis; nenhum download implícito em CI. |
| **`json.RawMessage` para argumentos de tool e JSON Schema** | Preserva bytes exatos; evita re-serialização que quebra round-trip. |
| **`Message` com chaves desconhecidas ordenadas** | Mapas Go não preservam ordem; o Python emite `role, content, timestamp, **kwargs`. Chaves não modeladas são retidas em ordem para round-trip fiel. |
| **`Content` distingue string de array** | `str \| list[dict]` no Python; string vazia e lista vazia são distintas no disco. |
| **`ToolResult` carrega erro como conteúdo** | Igual ao Python (`ToolResult(str)` com `is_error`): o modelo vê a falha e se recupera, em vez de o turno abortar. |
| **Compatibilidade semântica, não byte-a-byte** | O Python faz `json.loads` → dict, então ordem de chaves é irrelevante. Os testes diferenciais comparam JSON parseado. |

---

## 4. Milestones

| # | Resultado | Status |
|---|---|---|
| **M0** | Upstream congelado + harness de compatibilidade | ✅ feito |
| **M1** | Config engine + tipos centrais + sessões | 🔄 em andamento |
| **M2** | Provider OpenAI-compatible + streaming SSE | 🔄 em andamento |
| **M3** | AgentRunner + tool loop + execução paralela | 🔄 em andamento |
| **M4** | AgentLoop + MessageBus + CLI | 🔄 em andamento |
| **M5** | Tools nativas: filesystem, shell, web | 🔄 em andamento |
| **M6** | Telegram + gateway + cron | roadmap |
| **M7** | Memory + Dream + compaction | roadmap |
| **M8** | MCP client + plugins + subagents | roadmap |
| **M9** | Demais providers/canais + API + WebUI | roadmap |
| **M10** | Fuzzing, benchmarks, builds ARM/MIPS, paridade | roadmap |

---

## 5. Estratégia de multiagentes

O trabalho foi decomposto em módulos com fronteiras limpas, para que agentes
paralelos não colidam:

```
FASE RECON (paralela)          FASE BUILD (paralela)
┌──────────────────────┐       ┌──────────────────────────┐
│ spec-config          │       │ config engine            │
│ spec-session         │ ────► │ session JSONL store      │
│ spec-agent-core      │       │ provider openai-compat   │
└──────────────────────┘       │ tools (fs/shell/web)     │
                               └──────────────────────────┘
                                            │
              contratos congelados em código (não em prosa):
              internal/core     ← vocabulário
              internal/provider ← fronteira de modelo
              internal/tools    ← fronteira de ferramenta
```

**Regra de integração**: agentes paralelos só dependem de pacotes já
congelados. Nenhum agente edita pacote de outro. A integração (AgentLoop,
Runner, bus, CLI) é feita em série pelo agente principal, para evitar conflitos.

---

## 6. Testes e verificação

### Pirâmide

1. **Testes unitários Go** — por pacote, incluindo casos adversariais
   (conteúdo nulo, JSON malformado, argumentos não-objeto, chave inválida).
2. **Testes de round-trip** — escrever com Go, ler com Go, comparar JSON parseado.
3. **Testes diferenciais** — mesma entrada no Python e no Go, comparar efeitos
   (não texto do LLM). Roda onde o Python estiver disponível.
4. **Fixtures de compatibilidade** — `compat/fixtures/` com configs e sessões
   reais do formato upstream.

### O que NÃO se compara

Texto gerado pelo LLM (não determinístico). Compara-se protocolo e efeitos:

```
config carregada?          tool chamada corretamente?
session criada?            tool result reinjetado?
system prompt equivalente? stream correto?
tool schema equivalente?   memória escrita?
```

### Benchmarks

`benchmarks/` mede RSS, heap, goroutines, startup, tamanho do binário.
**Nenhum número de RAM será publicado antes de medido.**

---

## 7. Riscos

| Risco | Severidade | Mitigação |
|---|---|---|
| Escopo: 160k LOC Python | **Alta** | Milestones; entrega incremental verificada; nada de "100% pronto" sem matriz |
| Deriva do upstream (main ativo) | Média | Commit congelado é a especificação; upgrades são decisão explícita |
| `loop.py` com 104 KB concentra lógica não documentada | **Alta** | Spec derivada do código; testes diferenciais por comportamento |
| Formatos de sessão/checkpoint com detalhes não óbvios | Média | Spec + round-trip tests + fixtures reais |
| Canais (67k LOC, 18 canais) | **Alta** | Fora do escopo desta sessão; M6+ um canal por vez |
| Paridade de prompt de sistema | Média | Context builder portado a partir de `agent/context.py` + testes |

---

## 8. Reprodutibilidade

```bash
. scripts/goenv.sh          # toolchain pinado, GOTOOLCHAIN=local
go build ./cmd/nanobot      # binário
go test ./...               # testes
```

Sem rede em build. Sem dependências externas. `upstream/` é somente leitura.
