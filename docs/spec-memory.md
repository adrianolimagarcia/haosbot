# Spec — Memory/Dream (nanobot-go)

Fonte de verdade: `upstream/nanobot/nanobot/agent/memory.py` @ `1bb712d3`
(1.297 LOC). Este documento é a especificação operacional; onde houver dúvida,
**o Python congelado decide**, e a dúvida se resolve executando-o, não lendo.

Regra do projeto: zero dependências externas (só stdlib), Go 1.23.5,
`GOTOOLCHAIN=local`. Rodar sempre `. scripts/goenv.sh` antes de qualquer `go`.

---

## 1. Layout em disco (compatibilidade crítica)

Tudo relativo ao workspace do agente:

| Caminho | Papel |
|---|---|
| `memory/MEMORY.md` | memória de longo prazo |
| `memory/history.jsonl` | journal append-only |
| `memory/HISTORY.md` | formato legado, só leitura + migração |
| `memory/.cursor` | contador de cursor persistido |
| `memory/.dream_cursor` | cursor do último Dream |
| `SOUL.md` | persona |
| `USER.md` | perfil do usuário |

`memory/` é criado no construtor (`ensure_dir`), **não** sob demanda.

Arquivos versionados pelo GitStore: `SOUL.md`, `USER.md`, `memory/MEMORY.md`,
`memory/.dream_cursor` — exatamente esses quatro, nessa lista.

## 2. Formato de `history.jsonl`

Uma linha JSON por entrada, `ensure_ascii=False`:

```json
{"cursor": 1, "timestamp": "2026-02-14 09:31", "content": "..."}
```

- `cursor`: inteiro, monotonicamente crescente, começa em 1
- `timestamp`: `%Y-%m-%d %H:%M` — **naive local, sem segundos, sem timezone**.
  Não é ISO-8601. Este é o erro mais fácil de cometer aqui.
- `content`: texto já normalizado
- `session_key`: campo **opcional**, presente só quando fornecido

### Normalização de entrada (`_normalize_history_entry`)

1. `raw = entry.rstrip()`
2. `content = strip_think(raw)`
3. se `len(content) > limit` (default `_HISTORY_ENTRY_HARD_CAP = 64000`):
   trunca com `truncate_text(content, limit)` e loga **uma única vez**
   (rate-limit por flag `_oversize_logged`)

Caso sutil: se `raw` é não-vazio mas `content` fica vazio após `strip_think`,
persiste **string vazia**, não o raw. O comentário no código explica: reverter
para o raw desfaria as garantias do `strip_think` quando o Dream consumir.

### Alocação de cursor (`_next_cursor`)

Precisa ser atômica junto com o append (lock). Algoritmo:

```
contador = ler(.cursor)              # None se ausente/inválido/negativo
ultimo   = cursor da última entrada  # None se ausente/inválida
se contador existe:
    se ultimo existe:      return max(contador, ultimo) + 1
    senao:                 return max(contador, max_todos_cursores) + 1
senao:
    se ultimo existe:      return ultimo + 1
    senao:                 return max_todos_cursores + 1     # default 0
```

O caminho rápido confia na cauda; se ela estiver corrompida, varre o arquivo
inteiro e pega o máximo — o que permanece correto mesmo se a invariante
monotônica foi quebrada por escrita externa.

### Entradas inválidas

`_iter_valid_entries` **descarta** entradas com cursor inválido ou payload
malformado, logando uma vez cada tipo (`_corruption_logged`,
`_malformed_entry_logged`). Descartar, não falhar.

## 3. Migração de `HISTORY.md`

Roda no construtor. É one-time: se `history.jsonl` já existe, não faz nada.

Precisa de: `_parse_legacy_history`, `_split_legacy_history_chunks`,
`_should_start_new_legacy_chunk`, `_is_raw_legacy_chunk`,
`_legacy_fallback_timestamp`, `_next_legacy_backup_path`.
O arquivo legado é **preservado** como backup numerado, não apagado.

Falha na migração é logada (`logger.exception`) e engolida — não impede o
MemoryStore de existir.

## 4. Compactação

`compact_history()` mantém no máximo `max_history_entries` (default 1000),
preservando a cauda e **reescrevendo atomicamente** (`_write_entries`).

## 5. Dream

- `get_last_dream_cursor` / `set_last_dream_cursor`
- `dream_prompt_file()` → `memory/dream.md` (override)
- `has_dream_prompt_override()`
- `default_dream_prompt()` → renderiza `agent/dream.md` com
  `skill_creator_path=BUILTIN_SKILLS_DIR/"skill-creator"/"SKILL.md"`
- `build_dream_prompt(max_entries=20)` → `(prompt, cursor) | None`
- `dream_content_diff()` → `GitStore.summarize_working_tree` sobre
  `("SOUL.md", "USER.md", "memory/MEMORY.md")`
- `build_dream_tools()` → registry de ferramentas do Dream

---

## 6. Ordem de implementação

1. `MemoryStore` — I/O puro: paths, read/write dos 3 arquivos, `get_memory_context`
2. `history.jsonl` — normalização, cursor, append atômico, iteração válida
3. Compactação
4. Migração legada
5. `GitStore` (se ainda não existir no port) — verificar antes
6. Dream: cursor, prompt, diff, tools
7. O consolidador (o resto do arquivo, ~700 LOC): consolidação de checkpoints

## 7. Verificação obrigatória

- Testes unitários por comportamento, com os casos de borda acima
- **Diferencial**: estender `compat/python/dump_reference.py` para dumper os
  formatos e rodar o Python para capturar a verdade; comparar em
  `compat/differential_test.go`
- **Interop**: escrever com Go, ler com Python, e vice-versa — como já existe
  para sessions (`compat/python/interop_session.py`)
- `go build ./... && go vet ./... && gofmt -l` limpos, `-race` verde, e2e verde

Um teste que assume comportamento sem rodar o Python é a fonte de erro mais
comum neste projeto: **três vezes** o teste estava errado e o código certo.
Sempre executar a referência antes de "corrigir" o port.
