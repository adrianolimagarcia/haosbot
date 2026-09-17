# Benchmarks — nanobot-go

> Todos os números neste documento foram **medidos** nesta máquina. Nenhum é
> estimativa, projeção ou valor de marketing. Onde uma medição falhou, isso
> está declarado em vez de substituído por um palpite.

## Ambiente

| Item | Valor |
|---|---|
| CPU | Intel(R) Core(TM) i7-7700HQ @ 2.80GHz (8 threads) |
| SO | Linux x86_64 |
| Toolchain | Go 1.23.5 (`GOTOOLCHAIN=local`) |
| Build | `CGO_ENABLED=0 -trimpath -ldflags="-s -w"` |

Reproduzir:

```bash
. scripts/goenv.sh
./scripts/bench.sh          # perfil do binário + benchmarks de biblioteca
./scripts/bench.sh --json   # apenas números, em JSON
```

---

## 1. Perfil do binário

| Métrica | Valor medido | Como foi medido |
|---|---:|---|
| Tamanho do binário | **6,7 MB** (6.979.736 bytes) | `stat -c%s` |
| Startup (melhor de 20) | **2,65 ms** | `nanobot version` cronometrado 20×, mínimo |
| RSS de pico | **7,31 MB** (7.484 KB) | `VmHWM` de `/proc/<pid>/status` |

**Estes números substituem uma medição anterior de 1,7 MB / 0,99 ms / 4,04 MB.**
Aquela medição era do binário **stub**, antes de config, session, provider e
tools existirem. Os valores atuais incluem o runtime real. Publicar os antigos
como se ainda valessem seria enganoso, então a tabela foi corrigida em vez de
mantida.

Nota: o `selftest` constrói bus, registry de tools, context builder e runner,
mas **não** abre rede nem carrega canais. Um gateway com canais ativos usará
mais.

### O que o RSS de 7,31 MB realmente mede

O processo constrói o runtime real — MessageBus, ToolRegistry, ContextBuilder e
AgentRunner — e então fica ocioso, para que `/proc` possa ser amostrado.

**Limitação declarada:** um processo que sai em ~1 ms não pode ser amostrado por
um poller externo. A primeira versão deste script reportou `0.00 MB` porque
media `nanobot version`, que termina antes de qualquer amostra. Isso não era uma
medição — era um artefato. O comando `selftest --hold` existe só para tornar a
medição possível, e o script agora **falha explicitamente** em vez de reportar
zero.

**O que este número NÃO inclui:** canais (Telegram, Discord, …), servidores MCP,
subagents, WebUI, nem nenhum carregamento de modelo. Um gateway com canais
ativos usará mais. Nenhum número de RSS de gateway é publicado aqui porque
nenhum gateway foi implementado ainda.

### Referência Python

**Não há comparação.** O nanobot Python não pôde ser executado neste ambiente
(`filelock`, `loguru` e `tiktoken` não estão instalados, então o pacote não
importa). Sem uma medição do lado Python, qualquer afirmação de "X vezes menos
RAM" seria inventada. A comparação fica pendente até o harness diferencial
existir.

---

## 2. Benchmarks de biblioteca

`go test -bench=. -benchmem -benchtime=200x ./benchmarks/`

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| `PromptBuildSmall` | 23.097 | 10.936 | 28 |
| `PromptBuildWithSkills` (50 skills) | 235.875 | 53.025 | 353 |
| `MessageMarshal` | 21.146 | 3.894 | 22 |
| `MessageUnmarshal` | 23.320 | 3.960 | 106 |
| `BusPublishConsume` | 347,1 | 256 | 2 |
| `BusPublishParallel` | 406,7 | 544 | 1 |
| `RegistryExecute` | 231,1 | 5 | **0** |
| `RegistrySchemas` (20 tools) | 3.056 | 1.632 | 21 |
| `RunnerSingleTurn` | 759,6 | 784 | 4 |
| `RunnerToolLoop` (1 ida e volta de tool) | 4.702 | 2.517 | 20 |
| `RunnerLargeTranscript` (200 mensagens) | 55.147 | 139.424 | 4 |

### Observações

- **`RegistryExecute` com 0 allocs/op** — o caminho de despacho de tool não
  aloca. O resultado é o objetivo do design: `Registry.Execute` é chamado uma
  vez por tool call por iteração.
- **`RunnerLargeTranscript`: 139 KB/op** para 200 mensagens. Esse é o custo de
  copiar a fatia de mensagens no início de cada `Run`, para não mutar a fatia do
  chamador. É o maior alocador do conjunto e um alvo óbvio de otimização futura,
  mas só deve ser otimizado depois de medir um perfil real de uso — copiar a
  fatia é o que garante que `Run` seja seguro para reuso concorrente do mesmo
  histórico.
- **`PromptBuildWithSkills` é ~10× `PromptBuildSmall`** porque faz varredura de
  diretório em `skills/*/SKILL.md` a cada construção. O Python também reconstrói
  o prompt por turno, então isso é fiel; um cache invalidado por mtime seria uma
  otimização segura se virar gargalo.
- **`BusPublishParallel` não é mais rápido que o sequencial** nesta carga. Isso
  é esperado: o bus é protegido por mutex e o benchmark mede contenção de lock,
  não throughput útil. A fila do Python é um `asyncio.Queue` em loop único, então
  não há paralelismo real lá tampouco.

### Ressalvas metodológicas

- `-benchtime=200x` (poucas iterações) mantém a execução rápida. Os números têm
  variância de execução para execução; a tabela acima é de uma execução, não de
  uma mediana de várias. Para comparar antes/depois de uma mudança, rode ambos
  com `-count=10` e compare medianas.
- Benchmarks de runner usam providers falsos, então medem o framework, não a
  rede. Nenhum número de latência de LLM é reivindicado.

---

## 3. Metas não medidas

O plano do projeto menciona um perfil `tiny` e builds ARM/MIPS. **Nenhum dos
dois existe ainda**, então nenhum número é publicado. Seriam afirmações sem
evidência.
