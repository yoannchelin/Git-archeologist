# Claude.md — Git Archaeologist

> Briefing pour reprendre ce projet dans une nouvelle session Claude.
> Lis ce fichier *en entier* avant d'écrire la moindre ligne. Il contient les décisions déjà prises et les pièges à ne pas refaire.

---

## 1. Le projet en une phrase

**Git Archaeologist** est un **serveur MCP** qui indexe un repo **Go ou TypeScript** et permet à n'importe quel client MCP (Claude Desktop, Zed, Cursor…) de comprendre ce repo en posant des questions en langage naturel. Tout tourne **en local** via **Ollama** — aucune ligne de code ne quitte la machine.

Use case prioritaire : **onboarding** — un dev rejoint une équipe, balance le repo à l'agent, et peut demander *"où est géré le paiement ?"* ou *"quels fichiers toucher pour ajouter un nouveau provider d'auth ?"*.

---

## 2. Décisions architecturales — ne pas remettre en cause sans bonne raison

Ces choix ont été pris consciemment au début du projet. Si tu veux en changer un, **demande à l'utilisateur d'abord**.

| Décision | Raison |
|---|---|
| **Go comme langage primaire** | Niche-down. Le marché Go (k8s, Terraform, infra) est gros et mal outillé. Seul Go bénéficie du call graph typé. |
| **TypeScript via regexp** (S1 livré) | Pas de tree-sitter : zéro CGo, zéro grammar à maintenir. On extrait fonctions/classes/interfaces/imports au niveau top-level — suffisant pour l'onboarding. Call graph TS = S2. |
| **`go/packages` + `go/types`**, pas tree-sitter | Tree-sitter perd la résolution de types → pas de call graph précis ni d'implémentations d'interfaces. C'est le différenciateur vs RAG générique. |
| **MCP server stdio** comme interface primaire | Branchable partout. VSCode / web dashboard = S2. |
| **Ollama** comme LLM, pas Claude API | Demande du user : tout en local, repos confidentiels OK. Llama.cpp / API distante = derrière l'interface `llm.Client`. |
| **SQLite unique** (graph + FTS5 + embeddings + git) | Zero infra. Cosine via fonction C enregistrée (`store/vec.go`) — OK jusqu'à ~250k symboles. sqlite-vec HNSW si on dépasse. |
| **Retrieval hybride** : vector + FTS5 + graph expansion | C'est le secret sauce. Le RAG naïf rate "payment" si le code dit `ChargeCustomer()`. Le graph rattrape. Voir `internal/retrieve/retrieve.go`. |
| **Embedder funcs + types + interfaces + files**, pas packages ni vars/consts | Sweet spot granularité/coût. |
| **Texte d'embedding composite** = `kind + qualified + signature + doc + ~80 premières lignes de body` | Doc = intent, signature = shape, code prefix = structure. Pas tout le body : dilue le vecteur. |
| **Graph expansion par défaut = callers (incoming)** | Pour les questions "où est X handled ?", le caller (le handler HTTP) est plus informatif que le callee (le client lib). |
| **Use case prioritaire : onboarding** | Décide les arbitrages quand il y a des tensions (vitesse vs exhaustivité, etc.). |

---

## 3. Stack technique exacte

- **Go 1.25+** (requis par MCP SDK v1.4.x). Downgrade SDK à v1.2.0 si Go 1.23/1.24.
- **`github.com/modelcontextprotocol/go-sdk` v1.4.1** — SDK officiel Go pour MCP. API : `mcp.NewServer`, `mcp.AddTool`, `&mcp.StdioTransport{}`. Schémas inférés depuis les structs via tags `jsonschema`.
- **`github.com/mattn/go-sqlite3`** — driver SQLite avec FTS5 compilé. Pas de cgo problématique sur les plateformes classiques.
- **`golang.org/x/tools/go/packages`** + **`go/types`** + **`go/ast`** pour le parsing.
- **`github.com/go-git/go-git/v5`** — git pur-Go. La sentinelle pour arrêter `ForEach` est `storer.ErrStop` (sous `plumbing/storer`).
- **Ollama** — `nomic-embed-text` (768 dims) pour les embeddings, `qwen2.5-coder:14b` pour le chat. API HTTP locale `http://127.0.0.1:11434`.

---

## 4. Layout — où se trouve quoi

```
cmd/
  archaeo/          CLI : `archaeo index|info|query` (debug + indexation)
  archaeo-mcp/      Binaire serveur MCP (stdio transport)
internal/
  store/            SQLite : schéma + helpers typés + embeddings storage
                    - schema.go : DDL complet (tables + FTS5 + triggers)
                    - store.go  : Open, BatchInsert, SearchFTS, Neighbors,
                                  NearestNeighbors (cosine via SQL function)
                    - vec.go    : driver "sqlite3_archaeo" + cosine_sim() C func
                    - sort.go   : helper de tri
  parser/           go/packages → symboles + edges (Go)
                    typescript.go → symboles + import edges (TS/TSX, regexp)
                    Passe 1 : files + symbols
                    Passe 2 : edges (calls, implements pour Go ; imports pour TS)
                    - parser_test.go : smoke test sur testdata/sample
  gitlog/           go-git → commits + churn par fichier
                    HotFiles() = top fichiers par churn (proxy de risque)
  llm/              Client Ollama : Embed() + Chat()
                    Endpoint legacy /api/embeddings, pas /api/embed
  embed/            Pipeline d'embedding (sélection + composition + persist)
  retrieve/         Retrieval hybride : vector + FTS + graph expand + rerank
                    Voir Query() et DefaultOptions()
  index/            Orchestrateur : parser → gitlog → embed
  mcpserver/        Les 5 outils MCP
testdata/sample/    Petit repo Go (payment.go) pour les tests
```

**Schéma SQLite** (clé du système) : `files`, `symbols`, `edges`, `embeddings`, `symbols_fts` (FTS5 contentless mirror de `symbols`), `commits`, `file_commits`, `meta`. `files` a une colonne `language` (`go` / `typescript`). Relations dans `edges` : `calls`, `implements`, `contains`, `imports`, `embeds`. Détail complet dans `internal/store/schema.go`.

---

## 5. Les 6 outils MCP exposés

Définis dans `internal/mcpserver/server.go` et `internal/mcpserver/diagram.go`. Si tu en ajoutes un, **résiste à la prolifération** — la valeur vient de ces 6 qui couvrent le cycle d'onboarding.

| Tool | Quand l'utiliser |
|---|---|
| `query` | Question NL → top symboles. Le défaut. |
| `find_entrypoints` | Démarrage d'onboarding : `main()`, `init()`, routes HTTP. |
| `explain_symbol` | Deep dive sur un symbole : signature, doc, callers, callees, implémentations. |
| `where_to_add` | "Où ajouter X ?" → fichiers candidats. |
| `architecture_overview` | Vue top-down : packages + hot files. |
| `diagram` | Diagramme Mermaid du call graph autour d'un symbole (`kind=call_graph`) ou des dépendances inter-packages (`kind=package_deps`). Rendu inline dans Claude Desktop. |

---

## 6. État actuel — ce qui marche, ce qui manque

### ✅ Implémenté et testé en exécution
- Schéma SQLite complet avec FTS5 + triggers de sync
- Parser Go avec call graph et impl edges (interfaces), `imports` et `embeds`
- Parser TypeScript/TSX (regexp, zéro CGo) — fonctions, classes, interfaces, type aliases, import edges
- Ingest Git avec churn par fichier
- Client Ollama (embed + chat)
- Pipeline d'embedding + `RunPackage` pour embedding incrémental par package
- Retrieval hybride (vector + FTS + graph) avec freshness boost (+0.15 pour fichiers récents)
- Orchestrateur d'indexation + watcher `fsnotify` incrémental
- CLI `archaeo` (index / info / query)
- Serveur MCP stdio avec les 6 tools (dont `diagram`)
- `find_entrypoints` étendu : HTTP routes + Cobra commands + gRPC services
- Cosine similarity via fonction C SQLite (`store/vec.go`) — ~10× plus rapide, zéro allocation Go
- Smoke test sur `testdata/sample/payment.go` (`make test` passe)
- Testé sur microsoft/TypeScript (21 121 fichiers, 3.2s, 2025 import edges)
- README + Makefile (avec `-tags fts5`)

### ❌ À faire (ordre = ROI décroissant)

1. ~~**Génération de diagrammes Mermaid**~~ — **FAIT**
2. ~~**Indexation incrémentale via `fsnotify`**~~ — **FAIT**
3. ~~**Cosine similarity en C via ConnectHook**~~ — **FAIT** (`store/vec.go`). Pour aller plus loin : sqlite-vec HNSW au-delà de ~250k symboles.
4. ~~**Plus de relations dans le graphe**~~ — **FAIT** (`imports`, `embeds`)
5. ~~**Détection d'entrypoints plus fine**~~ — **FAIT** (Cobra, gRPC ajoutés). Reste : schedulers (`robfig/cron`), workers (`go func()` dans `main`).

6. ~~**Tests d'intégration sur 3 vrais repos Go**~~ — **FAIT**. Résultats :

   | Repo | Files | Symbols | Call edges | Impl edges | Temps |
   |---|---|---|---|---|---|
   | Hugo | 500 | 7 976 | 7 721 | 2 660 | 18s |
   | Terraform | 1 200 | 16 672 | 21 117 | 1 806 | **63s** ⚠️ |
   | Kubernetes | 3 358 | 43 475 | 44 572 | 2 638 | 11s |

   Qualité FTS ~65-70% sans embeddings (scheduler k8s, state Terraform : 3/3 ; shortcodes Hugo, API server k8s : 1/3). Embeddings indispensables pour les queries sémantiques ambiguës.

7. ~~**Re-ranker plus malin**~~ — **FAIT** : PageRank (`store/pagerank.go`, 20 itérations, +0.2×score) + freshness (+0.15). Reste : centralité par in-degree brut si PageRank est trop lent sur très gros graphes.

8. ~~**Support des `_test.go`**~~ — **FAIT** : flag `--with-tests` sur `archaeo index` et `archaeo-mcp`. Symbols marqués `is_test`, pénalité 0.4× en retrieval.

9. ~~**`--fast` mode (skip `NeedDeps`)**~~ — **FAIT** : flag `--fast` sur `archaeo index` et `archaeo-mcp --fast`. `ParseConfig.Fast` retire `NeedDeps|NeedTypes|NeedTypesInfo` de `packages.Config.Mode`. Résultat : Terraform 63s → ~15s, au prix de l'absence de call/impl edges cross-package. À utiliser pour découverte rapide.

10. ~~**go.work multi-module**~~ — **FAIT** : `parseGoWorkDirs()` dans `index/index.go` lit les directives `use` de `go.work`. `Build` itère chaque module dir en passant `ParseConfig.LoadDir`. Les chemins de fichiers restent relatifs à `repoRoot` (unchanged). Kubernetes/staging maintenant entièrement couvert.

11. **Call graph TypeScript** — actuellement import edges seulement. Approche : scan heuristique des corps de fonctions pour les call sites (`identifier(`), résolution within-file d'abord, puis same-package. Cross-file via import edges déjà existants.

---

## 7. Pièges connus / leçons apprises

- **Ne pas mettre de logs sur stdout** dans `archaeo-mcp` — stdout est le wire MCP. Tout va sur stderr.
- **Ne pas paralléliser les requêtes Ollama** — il sérialise en interne, le parallélisme cause du thrash GPU. Pour la vitesse, utiliser `/api/embed` (multi-input batch) à la place de `/api/embeddings`.
- **Les structs anonymes passées à des fonctions génériques** marchent en Go mais sont fragiles. Toujours nommer le type (cf. `NeighborHit`, `idScore`).
- **Les méthodes Go** : `pkg.TypesInfo.Defs[fn.Name]` retourne un `*types.Func` ; pour les types c'est `*types.TypeName`. La map `symIDs[types.Object]int64` accepte les deux car `types.Object` est une interface.
- **`go-git` est ~3× plus lent que C git** sur les gros repos. C'est OK pour onboarding (cap à 5000 commits). Pas OK pour CI continue → S2.
- **FTS5 n'aime pas la ponctuation** dans la query. La fonction `buildFTSQuery` dans `retrieve.go` filtre et quote chaque token. Si tu changes le tokenizer dans `schema.go`, vérifie cette fonction aussi.
- **Le parser fait deux passes** : pass 1 = symboles, pass 2 = edges, avec un Commit entre. Pourquoi : un parse error sur un package au pass 2 ne doit pas poisonner les symboles déjà persistés.
- **`go/packages` charge tous les imports avec `NeedDeps`**. Sur un gros repo, ça peut consommer beaucoup de RAM. À surveiller, possibilité de retirer `NeedDeps` si problème (au prix de la résolution cross-package).
- **`go-git` `ForEach` sentinel** : c'est `storer.ErrStop` (depuis `plumbing/storer`), pas une erreur custom. Erreur fréquente.
- **`make build` ne mettait pas à jour `bin/`** — le target compilait avec `go build ./...` sans `-o`. Maintenant `build` délègue à `bin/archaeo` et `bin/archaeo-mcp`. Toujours vérifier que le bon binaire est utilisé lors du debug.
- **TypeScript ESM : les imports `.js` pointent vers des fichiers `.ts`** — depuis TS 4.x en mode ESM, les import specifiers utilisent `.js` même si le fichier source est `.ts` (ex: `from "./checker.js"` → `checker.ts`). `resolveImport` dans `typescript.go` gère ça en strippant `.js` et cherchant `.ts`/`.tsx`. Ne pas oublier ce cas si on touche la résolution d'imports TS.
- **Les fichiers de test TS ne suivent pas tous le pattern `_test.go`** — il faut détecter les dossiers (`tests/`, `__tests__`, `spec/`, `e2e/`, `__mocks__`) ET les suffixes (`.test.ts`, `.spec.ts`). Voir `isTSTestFile()` dans `typescript.go`. Sans ça, les milliers de cas de test du repo TypeScript (microsoft/TypeScript) noient les vrais résultats de retrieval.
- **`cosine_sim` enregistré via ConnectHook** : le driver s'appelle `"sqlite3_archaeo"` (pas `"sqlite3"`). Si tu ajoutes d'autres tests ou outils qui ouvrent une DB directement, ils doivent utiliser ce driver, sinon `cosine_sim` n'est pas disponible et les queries vectorielles échouent.
- **FTS seul ne suffit pas pour le sémantique** — testé sur microsoft/TypeScript : "where is parsing done" ne trouve pas `parser.ts` en #1 sans embeddings. Le FTS stemme "parsing" → "pars" mais les 20k fichiers de test avec "pars" dans le nom noient le signal. Les embeddings (Ollama) sont indispensables pour la qualité sémantique.
- **Terraform lent à indexer** — résolu via `--fast` flag (`ParseConfig.Fast`). Sans `NeedDeps|NeedTypes|NeedTypesInfo`, le type-checker externe est éliminé : ~15s au lieu de 63s. Contrepartie : pas de call/impl edges cross-package. Pour les gros repos avec beaucoup de dépendances externes, recommander `--fast` en première passe.
- **Repos multi-modules avec `go.work`** — résolu : `parseGoWorkDirs()` dans `index/index.go` détecte `go.work` et itère chaque module. La fonction `Build` passe `ParseConfig.LoadDir` pour chaque répertoire de module. Kubernetes/staging entièrement couvert. Si `go.work` absent : comportement single-module inchangé.

---

## 8. Comportements attendus de toi (Claude) sur ce projet

### Style de code
- **Code Go idiomatique**. Pas de OOP gratuit. Petites interfaces (1-3 méthodes max).
- **Commentaires en haut de chaque fichier expliquant le *pourquoi***, pas juste le *quoi*. Les commentaires existants suivent ce pattern — garde-le.
- **Aucun TODO/FIXME laissé en place** sauf accord explicite de l'utilisateur. Soit on fait, soit on liste dans ce fichier.
- **Erreurs wrappées avec `fmt.Errorf("...: %w", err)`**, jamais retournées nues sans contexte.

### Style de communication
- **Réponses concises**. L'utilisateur préfère le punch à la verbosité. Pas de listes à puces dans les réponses conversationnelles sauf nécessité.
- **Tu peux pousser back**. Si l'utilisateur propose un truc qui contredit les décisions de la section 2 sans justification, dis-le clairement.
- **Tu travailles directement, sans préambule.** Pas de "Excellente question !", pas de "Voici ce que je propose…", pas de récap inutile à la fin.

### Quand modifier le code
- **Toujours `view` le fichier juste avant `str_replace`** — le code peut avoir bougé depuis le dernier état que tu connais.
- **Au moindre doute sur une API tierce**, fais une `web_search` plutôt que de deviner. Le SDK MCP Go évolue vite (v1.4.1 en mars 2026), les exemples obsolètes traînent.
- **Lance `make test` après chaque modif structurelle.** Le smoke test sur `testdata/sample` est rapide et attrape 80% des régressions.

### Quand ajouter une feature
- **Vérifie d'abord la section 6** pour savoir où elle se place dans le ROI.
- **Une feature = un commit logique**. Ne mélange pas deux changements indépendants.
- **Met à jour le README** si tu changes la surface publique (CLI flags, MCP tools).
- **Met à jour CE fichier** (`Claude.md`) si tu prends une nouvelle décision architecturale ou si tu apprends un nouveau piège.

---

## 9. Commandes utiles

```bash
# Premier démarrage
sed -i '' 's|github.com/yourname/git-archaeologist|github.com/TONUSER/git-archaeologist|g' \
  go.mod $(find . -name '*.go')
go mod tidy
make test                       # smoke test sur testdata/sample
make build                      # bin/archaeo, bin/archaeo-mcp

# Dogfooding
./bin/archaeo index --repo . --no-embed
./bin/archaeo info  --repo .
./bin/archaeo query --repo . "where is the call graph built"

# Avec embeddings (requiert Ollama running)
ollama pull qwen2.5-coder:14b
ollama pull nomic-embed-text
./bin/archaeo index --repo /path/to/big/repo

# Brancher sur Claude Desktop
# Édite ~/Library/Application Support/Claude/claude_desktop_config.json :
# {
#   "mcpServers": {
#     "archaeo": {
#       "command": "/abs/path/bin/archaeo-mcp",
#       "args": ["--repo", "/abs/path/to/repo"]
#     }
#   }
# }
```

---

## 10. Comment reprendre dans une nouvelle session

1. Lis ce fichier **en entier**.
2. Demande à l'utilisateur : *"On reprend où ? J'ai vu dans Claude.md qu'il reste X, Y, Z à faire — tu veux attaquer lequel, ou tu as un autre angle ?"*
3. Si l'utilisateur dit *"continue"* sans préciser, propose l'item 6 de la section 6 (tests d'intégration Go) ou l'item 7 (PageRank) — c'est le meilleur ROI restant.
4. Avant d'écrire du code, `view` les fichiers concernés pour t'aligner sur le code actuel.
5. Travaille. Mets à jour `Claude.md` à la fin si tu as appris quelque chose.
