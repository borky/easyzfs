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

// reSpanishText — a literal that reads as Spanish prose: an accented letter,
// or a Spanish word that English never uses.
var reSpanishText = regexp.MustCompile(`[áéíóúñÁÉÍÓÚÑ¿¡]|\b(el|la|los|las|del|al|lo|su|sus|para|con|sin|está|están|hay|puede|debe|que|una|uno|un|es|son|se|no|ningún|ninguna|ya|aún|también|desde|hasta|cuando|porque|sobre|entre|borrar|crear|leer|montar|desmontar|guardar|contraseña|usuario|entrada|inválid[ao]s?|válid[ao]s?|conflicto|papelera|almacenamiento|sesión|operación|disco|discos|réplica|origen|destino|nombre|clave|cifrado|puerto|ruta|fichero|archivo|encontrad[ao]s?|requerid[ao]s?|inexistente|desconocid[ao]s?|vací[ao]|demasiad[ao]s?|modo|solo|falta|faltan|falló|cancelad[ao]|tiene|tienen|existe|permitid[ao]|configurad[ao]|activ[ao]|pendiente|aviso|intento|intentos|petición|rechazad[ao]|cambiar|añadir|quitar|apagar|identificar|restaurar|mover|renombrar|sustituir|lanzar|comprobar|preparar|actualizaciones|nuevo|nueva|actual|propiedad|valor|tamaño|horas|día|minuto|semanal|mes|retención|tipo|terminado|iniciado|errores|en|nuevos|nuevas|histórico|estable)\b`)

var reSQL = regexp.MustCompile(`(?i)^\s*(SELECT|INSERT|CREATE|UPDATE|DELETE|ALTER|PRAGMA|WITH)\b`)

// notProse — literals the detector catches that are not user-visible text:
// identifiers, codes, English, and strings that never leave the process.
var notProse = map[string]bool{
	"es": true, "no": true, "tipo": true, "no-cache": true, "no such file": true,
	"No other update is running.": true, "No update in progress": true,
	"rollback: no backup available (.old not found)": true,
	"en": true, "inválid": true, // a language code; a substring test in httpapi
	`{"error":"streaming","message":"SSE no soportado"}`: true, // plain-text body of a failed SSE setup
	"job.create": true, "job.delete": true, "job.finished": true, "job.patch": true,
}

// skipFiles — text that is already bilingual, or never shown in the UI.
var skipFiles = []string{
	"internal/i18n/",           // this package
	"internal/push/i18n.go",    // push notifications: es/en templates
	"internal/notifier/",       // e-mail: es/en templates
	"internal/db/",             // migrations: logs and SQL
	"internal/priv/priv.go:38", // printed to a root terminal only
}

// userTexts — every Spanish-looking string literal outside log calls, SQL,
// struct tags and imports, in the packages whose text reaches the API.
func userTexts(t *testing.T) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	found := map[string]string{} // literal → first position
	root := "../../internal"
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return err
		}
		rel := "internal/" + strings.TrimPrefix(filepath.ToSlash(p), "../../internal/")
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			return err
		}
		skip := map[*ast.BasicLit]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
					if id, ok := sel.X.(*ast.Ident); ok && (id.Name == "log" || id.Name == "regexp" ||
						(id.Name == "fmt" && (strings.HasPrefix(sel.Sel.Name, "Print") || strings.HasPrefix(sel.Sel.Name, "Fprint")))) {
						ast.Inspect(x, func(m ast.Node) bool {
							if bl, ok := m.(*ast.BasicLit); ok {
								skip[bl] = true
							}
							return true
						})
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
			if err != nil || notProse[s] || reSQL.MatchString(s) || !reSpanishText.MatchString(s) {
				return true
			}
			pos := rel + ":" + strconv.Itoa(fset.Position(bl.Pos()).Line)
			for _, sf := range skipFiles {
				if strings.HasPrefix(pos, sf) {
					return true
				}
			}
			if _, ok := found[s]; !ok {
				found[s] = pos
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// Every user-visible Spanish string in the code has an English entry, so a
// UI set to English never shows Spanish. A new message fails here until it
// is added to catalogue.go.
func TestCatalogueCoversTheCode(t *testing.T) {
	texts := userTexts(t)
	if len(texts) < 300 {
		t.Fatalf("only %d texts found: the extractor no longer sees the code", len(texts))
	}
	var missing []string
	for s, pos := range texts {
		if _, ok := catalogue[s]; !ok {
			missing = append(missing, pos+"\t"+strconv.Quote(s))
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("%d user-visible Spanish strings have no English entry in catalogue.go:\n%s",
			len(missing), strings.Join(missing, "\n"))
	}
}

// Each English template uses the same placeholders as its Spanish key, and
// is actually English.
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
	for es, en := range catalogue {
		if strings.Join(verbs(es), ",") != strings.Join(verbs(en), ",") {
			t.Errorf("placeholders differ:\n  es %q\n  en %q", es, en)
		}
		if reSpanishMark.MatchString(en) {
			t.Errorf("English entry has Spanish letters: %q", en)
		}
		if _, err := compileEntry(es, en); err != nil {
			t.Errorf("%q: %v", es, err)
		}
	}
}
