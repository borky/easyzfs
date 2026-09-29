package i18n

import (
	"strings"
	"testing"
)

func TestSpanish(t *testing.T) {
	for en, want := range map[string]string{
		// A sentinel, a reason and a hint, each translated by its own entry.
		"disk in use: sdb is an LVM physical volume. If you really want to reuse it, wipe it by hand first (wipefs / zpool labelclear)": "disco en uso: sdb es un volumen físico LVM. Si de verdad quieres reutilizarlo, bórralo antes a mano (wipefs / zpool labelclear)",
		// Host storage with its hint appended.
		"host storage: rpool/data/imported-disk is used by VM 9001 in Proxmox (according to its configuration). Do it from the Proxmox interface, which knows which VM or container uses it": "almacenamiento del host: rpool/data/imported-disk lo usa VM 9001 en Proxmox (según su configuración). Hazlo desde la interfaz de Proxmox, que sabe qué VM o contenedor lo usa",
		// A single-word sentinel at the head of a wrapped error.
		"conflict: rpool/data/x already exists and is not a replica created by EasyZFS; nothing is received over it": "conflicto: rpool/data/x ya existe y no es una réplica creada por EasyZFS; no se recibe encima",
		// Numbers and %%.
		"Pool tank at 91% capacity (critical ≥ 90%)": "Pool tank al 91% de capacidad (crítico ≥ 90%)",
		// Command output (English, from zfs) passes through a wrapper.
		"delete snapshot: zfs: cannot destroy 'tank/a@s1': snapshot has dependent clones": "borrar snapshot: zfs: cannot destroy 'tank/a@s1': snapshot has dependent clones",
		// A phrase repeated with a one-character separator.
		"not available;not available": "no disponible;no disponible",
		// Nothing to translate.
		"tank/media": "tank/media", "ONLINE": "ONLINE", "": "", "42": "42",
	} {
		if got := Spanish(en); got != want {
			t.Errorf("Spanish(%q)\n got  %q\n want %q", en, got, want)
		}
	}
}

// A single word is translated only as the whole text or the head of an
// error, never inside a name; keys match whole words only.
func TestWholeWords(t *testing.T) {
	for in, want := range map[string]string{
		"tank/conflict":               "tank/conflict",
		"my conflict here":            "my conflict here",
		"unmount tank/a: busy":        "unmount tank/a: busy", // "mount %s: %v" must not match inside it
		"tank/a is not mounted: busy": "no se monta tank/a: busy",
	} {
		if got := Spanish(in); got != want {
			t.Errorf("Spanish(%q) = %q, want %q", in, got, want)
		}
	}
}

// Placeholders can be reordered in a translation.
func TestReorderedPlaceholders(t *testing.T) {
	e, err := compileEntry("%s's %s", "el %[2]s de %[1]s")
	if err != nil {
		t.Fatal(err)
	}
	if got := e.apply("tank's disk"); got != "el disk de tank" {
		t.Fatalf("got %q", got)
	}
}

func TestJSON(t *testing.T) {
	in := `{"error":"dev_in_use","message":"disk in use: sdb is an LVM physical volume","name":"conflict","path":"/tank/docs/does not exist.txt","command":"read log: x","used_bytes":18446744073709551615,"warnings":["tank/a is not mounted: busy"]}`
	out, ok := JSON([]byte(in))
	if !ok {
		t.Fatal("not JSON?")
	}
	s := string(out)
	for _, want := range []string{
		`"message":"disco en uso: sdb es un volumen físico LVM"`,
		`"name":"conflict"`,                      // a data field: never translated
		`"path":"/tank/docs/does not exist.txt"`, // an English phrase in a file name stays
		`"command":"read log: x"`,
		`"used_bytes":18446744073709551615`, // integers stay exact
		`"warnings":["no se monta tank/a: busy"]`,
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

func TestResolve(t *testing.T) {
	for _, c := range [][4]string{
		{"es", "en", "en", "es"}, {"auto", "es", "en", "es"}, {"auto", "", "es", "es"}, {"auto", "", "", "en"}, {"", "", "fr", "en"},
	} {
		if got := Resolve(c[0], c[1], c[2]); got != c[3] {
			t.Errorf("Resolve%v = %q, want %q", c[:3], got, c[3])
		}
	}
}
