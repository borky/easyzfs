package i18n

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// reSpanishText — a literal that reads as Spanish: an accented letter, or
// two Spanish words English never uses.
var reSpanishText = regexp.MustCompile(`(?i)[áéíóúñ¿¡]|\b(el|la|los|las|del|para|con|sin|está|están|hay|que|una|uno|ningún|ninguna|también|desde|hasta|cuando|porque|sobre|entre|borrar|crear|leer|montar|desmontar|guardar|usuario|entrada|conflicto|papelera|almacenamiento|disco|discos|origen|destino|nombre|clave|cifrado|puerto|ruta|fichero|encontrad[ao]|requerid[ao]|desconocid[ao]|vací[ao]|demasiad[ao]s?|modo|falta|faltan|falló|cancelad[ao]|tiene|tienen|existe|permitid[ao]|configurad[ao]|pendiente|aviso|intentos|petición|cambiar|añadir|quitar|apagar|restaurar|mover|renombrar|sustituir|lanzar|comprobar|actualizaciones|nuevo|nueva|actual|propiedad|valor|tamaño|horas|día|minuto|semanal|retención|tipo|terminado|iniciado|errores|nuevos|histórico|estable|tras|fuera|ejecutando|ejecutar|comando|hora|rango|importar|destruir|exportar|descartar|migrar|expandir|bloquear|desbloquear|clonar|listar|vaciar|respuesta|ilegible|umbral|mientras|ahora|antes|después|sesión|operación|leído|escrito|un|sale|importada|instalada|reiniciando|migraciones|por|como|más|muy|pero|será|genera|imprime|muestra|usa|pulsa|elige|instala|ejecuta|reinicia|comprueba|arrancado|parado|iniciando|esperando|reintentando|recibido|enviado|borrado|creado|montado|desmontado|guardado|cargado|listo|fallo|aviso|crítica)\b`)

var reSQL = regexp.MustCompile(`(?i)^\s*(SELECT|INSERT|CREATE|UPDATE|DELETE|ALTER|PRAGMA|WITH)\b`)

// spanishFiles, spanishOK — Spanish that is meant to stay: the Spanish side
// of what is bilingual by design, and identifiers that only look Spanish.
var spanishFiles = []string{
	"internal/i18n/",        // this package: the Spanish translations
	"internal/push/i18n.go", // push notifications: es/en templates
}

var spanishOK = map[string]bool{
	"crítica": true, "aviso": true, // notifier's Spanish severity labels
	"tipo": true, // a column and JSON field name
}

// noTranslate — messages that need no Spanish version: technical wrappers
// around English tool errors (zfs, sqlite, the updater), names and codes.
var noTranslate = map[string]bool{
	"smart test: %w": true, "scrub %s: %w": true, "trim: %w": true, "autotrim: %w": true,
	"checkpoint: %w": true, "vdev size: %w": true, "replace: %w": true, "set %s: %w": true,
	"rollback: %w": true, "zfs get properties: %w": true, "zfs set %s: %w": true,
	"zfs inherit %s: %w": true, "pvecfg: %w": true, "vacuum into: %w": true, "vacuum into: %v": true,
	"quick_check: %w": true, "quick_check: %s": true, "marshal: %w": true, "lsblk JSON: %w": true,
	"snapshot %s: %w": true, "bookmark %s: %w": true, "ssh: %s": true, "prune: %w": true,
	"updater: source: %w": true, "updater: config: %w": true, "updater: detect: %w": true,
	"updater: mkdir: %w": true, "updater: placeholder: %w": true, "updater: apply: %w": true,
	"updater: tag: %w": true, "updater: chmod: %w": true, "updater: flag: %w": true,
	"rollback: operation already in progress": true, "rollback: rename: %w": true,
	"rollback: chmod: %w": true, "rollback: flag: %w": true, "bootstrap admin: %w": true,
	"Pool %s DEGRADED": true, "Pool %s FAULTED": true,
	// smartctl's own verdicts and counters, shown as they are.
	"PASSED": true, "FAILED": true, "PASSED (realloc=2 pending=0)": true,
	"%s (realloc=%d pending=%d offunc=%d)": true, "%s (nvme warning=%d)": true,
	// Start-up and gateway failures: they reach the journal or a root
	// terminal, never a user who chose a language.
	"ping sqlite: %w": true, "migration %d: %w": true, "migration %d (record): %w": true,
	"migration %d (commit): %w": true, "the privileged gateway only runs as root (via sudo)": true,
	"usage: easyzfs priv <tool> <arguments…>": true,
	// Always wraps executil.ErrTimeout: translated by its full sentence
	// ("%s: timeout running the command after %s") instead, so a generic
	// "after" in some tool's English output is never touched.
	"%s: %w after %s": true,
}

// proseFields — struct fields whose text the UI shows.
var proseFields = map[string]bool{
	"SmartDetail": true, "Detail": true, "Message": true, "LastError": true, "Text": true,
	"Summary": true, "Title": true, "HostReason": true, "InUseReason": true, "Reason": true,
}

type literal struct {
	s, pos string
	msg    bool // a message: an error, an API message, an alert, a prose field
}

// literals — every string literal in the server's code, outside struct
// tags, imports, regexps and SQL.
func literals(t *testing.T) []literal {
	t.Helper()
	fset := token.NewFileSet()
	var out []literal
	var files []string
	err := filepath.Walk("../../internal", func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
			files = append(files, p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, "../../main.go")
	for _, p := range files {
		rel := strings.TrimPrefix(filepath.ToSlash(p), "../../")
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		skip := map[*ast.BasicLit]bool{}
		msgs := map[*ast.BasicLit]bool{}
		mark := func(n ast.Node, set map[*ast.BasicLit]bool) {
			ast.Inspect(n, func(m ast.Node) bool {
				if bl, ok := m.(*ast.BasicLit); ok && bl.Kind == token.STRING {
					set[bl] = true
				}
				return true
			})
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				switch fn := x.Fun.(type) {
				case *ast.SelectorExpr:
					if id, ok := fn.X.(*ast.Ident); ok {
						switch id.Name + "." + fn.Sel.Name {
						case "errors.New", "fmt.Errorf":
							mark(x.Args[0], msgs)
						case "regexp.MustCompile", "regexp.Compile":
							mark(x, skip)
						}
					}
					if fn.Sel.Name == "RaiseKind" && len(x.Args) > 4 {
						mark(x.Args[4], msgs)
					}
				case *ast.Ident:
					switch {
					case fn.Name == "writeErr" && len(x.Args) == 4:
						mark(x.Args[3], msgs)
					case fn.Name == "refuse" && len(x.Args) == 2:
						mark(x.Args[1], msgs)
					}
				}
			case *ast.KeyValueExpr:
				if id, ok := x.Key.(*ast.Ident); ok && proseFields[id.Name] {
					mark(x.Value, msgs)
				}
			case *ast.AssignStmt:
				for i, l := range x.Lhs {
					if sel, ok := l.(*ast.SelectorExpr); ok && proseFields[sel.Sel.Name] && i < len(x.Rhs) {
						mark(x.Rhs[i], msgs)
					}
				}
			case *ast.Field:
				if x.Tag != nil {
					skip[x.Tag] = true
				}
			case *ast.ImportSpec:
				skip[x.Path] = true
			}
			return true
		})
		ast.Inspect(f, func(n ast.Node) bool {
			bl, ok := n.(*ast.BasicLit)
			if !ok || bl.Kind != token.STRING || skip[bl] {
				return true
			}
			s, err := strconv.Unquote(bl.Value)
			if err != nil || reSQL.MatchString(s) {
				return true
			}
			out = append(out, literal{s: s, pos: rel + ":" + strconv.Itoa(fset.Position(bl.Pos()).Line), msg: msgs[bl]})
			return true
		})
	}
	return out
}

// hasWords — a literal with at least one word once its placeholders are gone.
func hasWords(s string) bool {
	return regexp.MustCompile(`\p{L}{2,}`).MatchString(reVerb.ReplaceAllString(s, ""))
}

// English is the product's language: no Spanish left in the server's code —
// messages, log lines, anything — outside the translations themselves.
func TestSourceIsEnglish(t *testing.T) {
	var bad []string
	for _, l := range literals(t) {
		skip := false
		for _, f := range spanishFiles {
			skip = skip || strings.HasPrefix(l.pos, f)
		}
		if !skip && !spanishOK[l.s] && reSpanishText.MatchString(l.s) {
			bad = append(bad, l.pos+"\t"+strconv.Quote(l.s))
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Errorf("%d Spanish literals in the code (write English; Spanish goes in catalogue.go):\n%s",
			len(bad), strings.Join(bad, "\n"))
	}
}

// Every message the server writes for people has a Spanish entry, so a user
// who selected Spanish never gets English. A new message fails here until it
// is added to catalogue.go (or to noTranslate, if it needs no translation).
func TestCatalogueCoversTheCode(t *testing.T) {
	var missing []string
	n := 0
	seen := map[string]bool{}
	for _, l := range literals(t) {
		if !l.msg || !hasWords(l.s) || noTranslate[l.s] || seen[l.s] {
			continue
		}
		seen[l.s] = true
		n++
		if _, ok := catalogue[l.s]; !ok {
			missing = append(missing, l.pos+"\t"+strconv.Quote(l.s))
		}
	}
	if n < 250 {
		t.Fatalf("only %d messages found: the scan no longer sees the code", n)
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("%d messages have no Spanish entry in catalogue.go:\n%s", len(missing), strings.Join(missing, "\n"))
	}
}

// Each translation uses the same placeholders as its key, and the keys are
// English.
func TestCatalogueEntriesAreWellFormed(t *testing.T) {
	verbs := func(s string) []string {
		var out []string
		for _, m := range reVerb.FindAllStringSubmatch(s, -1) {
			if m[4] != "%" {
				out = append(out, m[4])
			}
		}
		sort.Strings(out)
		return out
	}
	for en, es := range catalogue {
		if strings.Join(verbs(en), ",") != strings.Join(verbs(es), ",") {
			t.Errorf("placeholders differ:\n  en %q\n  es %q", en, es)
		}
		if reSpanishMark.MatchString(en) {
			t.Errorf("key is not English: %q", en)
		}
		if _, err := compileEntry(en, es); err != nil {
			t.Errorf("%q: %v", en, err)
		}
	}
}
