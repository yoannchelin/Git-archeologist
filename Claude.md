# Claude.md — Git Archaeologist

> Briefing pour reprendre ce projet dans une nouvelle session Claude.
> Lis ce fichier *en entier* avant d'écrire la moindre ligne. Il contient les décisions déjà prises et les pièges à ne pas refaire.

---

## 1. Le projet en une phrase

**Git Archaeologist** est un **serveur MCP** qui indexe un repo **Go** et permet à n'importe quel client MCP (Claude Desktop, Zed, Cursor…) de comprendre ce repo en posant des questions en langage naturel. Tout tourne **en local** via **Ollama** — aucune ligne de code ne quitte la machine.

Use case prioritaire : **onboarding** — un dev rejoint une équipe, balance le repo à l'agent, et peut demander *"où est géré le paiement ?"* ou *"quels fichiers toucher pour ajouter un nouveau provider d'auth ?"*.

---

## 2. Décisions architecturales — ne pas remettre en cause sans bonne raison

Ces choix ont été pris consciemment au début du projet. Si tu veux en changer un, **demande à l'utilisateur d'abord**.

| Décision | Raison |
|---|---|
| **Go, un seul langage** au MVP | Niche-down. Le marché Go (k8s, Terraform, infra) est gros et mal outillé. Multi-langage via tree-sitter = S2. |
| **`go/packages` + `go/types`**, pas tree-sitter | Tree-sitter perd la résolution de types → pas de call graph précis ni d'implémentations d'interfaces. C'est le différenciateur vs RAG générique. |
| **MCP server stdio** comme interface primaire | Branchable partout. VSCode / web dashboard = S2. |
| **Ollama** comme LLM, pas Claude API | Demande du user : tout en local, repos confidentiels OK. Llama.cpp / API distante = derrière l'interface `llm.Client`. |
| **SQLite unique** (graph + FTS5 + embeddings + git) | Zero infra. Brute-force cosine OK jusqu'à ~100k symboles. `sqlite-vec` ou Qdrant si on cogne un mur. |
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
                                  NearestNeighbors (brute-force cosine)
                    - sort.go   : helper de tri
  parser/           go/packages → symboles + edges
                    Passe 1 : files + symbols
                    Passe 2 : edges (calls, implements)
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

**Schéma SQLite** (clé du système) : `files`, `symbols`, `edges`, `embeddings`, `symbols_fts` (FTS5 contentless mirror de `symbols`), `commits`, `file_commits`, `meta`. Relations dans `edges` : `calls`, `implements`, `contains`. Détail complet dans `internal/store/schema.go`.

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
- Parser Go avec call graph et impl edges (interfaces)
- Ingest Git avec churn par fichier
- Client Ollama (embed + chat)
- Pipeline d'embedding
- Retrieval hybride (vector + FTS + graph)
- Orchestrateur d'indexation
- CLI `archaeo` (index / info / query)
- Serveur MCP stdio avec les 6 tools (dont `diagram`)
- Outil `diagram` : call graph Mermaid + package deps — testé via JSON-RPC stdio
- Smoke test sur `testdata/sample/payment.go` (`make test` passe)
- README + Makefile (avec `-tags fts5`)

### ❌ À faire (ordre = ROI décroissant)

1. ~~**Génération de diagrammes Mermaid**~~ — **FAIT** (`internal/mcpserver/diagram.go`)

2. **Indexation incrémentale via `fsnotify`** (1 journée) — détecter les modifs de fichiers `.go`, ré-parser le seul package touché, mettre à jour symboles + edges entrants/sortants. Essentiel dès qu'on dépasse la démo.

3. **`sqlite-vec` auto-load** (1/2 journée) — passer la recherche vectorielle de brute-force à indexée. Nécessaire au-delà de ~50k symboles. Le SQL change peu, juste l'extension à charger via `_extensions` dans le DSN sqlite3.

4. **Plus de relations dans le graphe** — actuellement on a `calls`, `implements`, `contains`. Ajouter `imports` (package → package), `uses` (func → var/const lus), `embeds` (struct → struct embarquée).

5. **Détection d'entrypoints plus fine** — actuellement heuristiques sur signature. Améliorer : détecter `mux.HandleFunc`, `gin.Engine.GET/POST/...`, `cobra.Command`, schedulers (`robfig/cron`), workers (`go func()` dans `main`).

6. **Tests d'intégration sur 3 vrais repos** : Kubernetes (énorme), Terraform (moyen), Hugo (petit). Mesurer : temps d'index, qualité du retrieval sur 10 questions templates.

7. **Re-ranker plus malin** — au-delà du score linéaire, intégrer la centralité du nœud dans le graphe (PageRank simple) et la fraîcheur Git (récent = plus pertinent).

8. **Support des tests** — actuellement on exclut `_test.go`. Les tests sont souvent la *meilleure* doc d'un module. À réintégrer avec un flag `--with-tests`.

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
3. Si l'utilisateur dit *"continue"* sans préciser, propose la feature 1 de la section 6 (diagrammes Mermaid) — c'est le meilleur ROI.
4. Avant d'écrire du code, `view` les fichiers concernés pour t'aligner sur le code actuel.
5. Travaille. Mets à jour `Claude.md` à la fin si tu as appris quelque chose.
