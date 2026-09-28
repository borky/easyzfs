package i18n

import (
	"strings"
	"testing"
)

func TestEnglish(t *testing.T) {
	for es, want := range map[string]string{
		// The report that started this: a sentinel, a reason and a hint,
		// each translated by its own entry.
		"disco en uso: sdb es un volumen físico LVM. Si de verdad quieres reutilizarlo, bórralo antes a mano (wipefs / zpool labelclear)": "disk in use: sdb is an LVM physical volume. If you really want to reuse it, wipe it by hand first (wipefs / zpool labelclear)",
		// Host storage with its hint appended.
		"almacenamiento del host: rpool/data/imported-disk lo usa VM 9001 en Proxmox (según su configuración). Hazlo desde la interfaz de Proxmox, que sabe qué VM o contenedor lo usa": "host storage: rpool/data/imported-disk is used by VM 9001 in Proxmox (according to its configuration). Do it from the Proxmox interface, which knows which VM or container uses it",
		// A single-word sentinel at the head of a wrapped error.
		"conflicto: rpool/data/x ya existe y no es una réplica creada por EasyZFS; no se recibe encima": "conflict: rpool/data/x already exists and is not a replica created by EasyZFS; nothing is received over it",
		// Numbers and %%.
		"Pool tank al 91% de capacidad (crítico ≥ 90%)": "Pool tank at 91% capacity (critical ≥ 90%)",
		// Command output (English already) passes through a wrapper.
		"borrar snapshot: zfs: cannot destroy 'tank/a@s1': snapshot has dependent clones": "delete snapshot: zfs: cannot destroy 'tank/a@s1': snapshot has dependent clones",
		// Nothing to translate.
		"tank/media": "tank/media", "ONLINE": "ONLINE", "": "", "42": "42",
	} {
		if got := English(es); got != want {
			t.Errorf("English(%q)\n got  %q\n want %q", es, got, want)
		}
	}
}

// A single Spanish word is translated only as the whole text or the head of
// an error, never inside a name.
func TestSingleWordsStayOutOfNames(t *testing.T) {
	for _, s := range []string{"tank/conflicto", "mi conflicto grande", "desconocidas"} {
		if got := English(s); got != s {
			t.Errorf("English(%q) = %q, want it untouched", s, got)
		}
	}
}

// Keys match whole words only: a key must not rewrite part of a longer word.
func TestKeysMatchWholeWords(t *testing.T) {
	for es, want := range map[string]string{
		"desmontar tank/a: busy":         "desmontar tank/a: busy", // not "de" + "mount tank/a…"
		"no se monta tank/a: busy":       "tank/a is not mounted: busy",
		"leer el estado de sdb: timeout": "read the state of sdb: timeout",
		"no disponible;no disponible":    "not available;not available",
	} {
		if got := English(es); got != want {
			t.Errorf("English(%q) = %q, want %q", es, got, want)
		}
	}
}

// Placeholders can be reordered in the English template.
func TestReorderedPlaceholders(t *testing.T) {
	e, err := compileEntry("el %s de %s", "%[2]s's %[1]s")
	if err != nil {
		t.Fatal(err)
	}
	if got := e.apply("el disco de tank"); got != "tank's disco" {
		t.Fatalf("got %q", got)
	}
}

func TestJSON(t *testing.T) {
	in := `{"error":"dev_in_use","message":"disco en uso: sdb es un volumen físico LVM","name":"conflicto","path":"/tank/docs/no existe.txt","command":"leer log: x","used_bytes":18446744073709551615,"warnings":["no se monta tank/a: busy"],"host_reason":"%s"}`
	out, ok := JSON([]byte(in))
	if !ok {
		t.Fatal("not JSON?")
	}
	s := string(out)
	for _, want := range []string{
		`"message":"disk in use: sdb is an LVM physical volume"`,
		`"name":"conflicto"`,                // a data field: never translated
		`"path":"/tank/docs/no existe.txt"`, // a Spanish phrase in a file name stays
		`"command":"leer log: x"`,
		`"used_bytes":18446744073709551615`, // integers stay exact
		`"warnings":["tank/a is not mounted: busy"]`,
		`"error":"dev_in_use"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s in %s", want, s)
		}
	}
	if _, ok := JSON([]byte("not json")); ok {
		t.Error("non-JSON accepted")
	}
}
