// props_test.go — whitelist de propiedades (U3): validadores, get/set/inherit
// con un fake zfs que emite fixtures y registra sus argumentos.
package actions

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newPropsTestService — como newTestService pero con un fake `zfs` en el PATH
// que: en "get" emite el fixture de propiedades, y en "set"/"inherit" anota
// los args (tras quitar flags tipo -H/-o). Cada invocación de set/inherit
// además se registra en el mismo log para verificar audit y ejecución.
func newPropsTestService(t *testing.T) (*Service, string) {
	t.Helper()
	dir := t.TempDir()
	logFile := filepath.Join(dir, "zfs-args.log")

	// zfs falso: get → emite el fixture; set/inherit → anota y sale 0.
	zfs := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  get) cat <<'EOF'\n" + fixtureProps + "EOF\n exit 0 ;;\n" +
		"  set|inherit) shift 1 ; echo \"$@\" >> \"$ZFS_LOG\" ; exit 0 ;;\n" +
		"  *) exit 1 ;;\n" +
		"esac\n"
	sudo := "#!/bin/sh\nwhile [ $# -gt 0 ]; do case \"$1\" in -*) shift;; *) break;; esac; done\nexec \"$@\"\n"
	for name, body := range map[string]string{"zfs": zfs, "sudo": sudo} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ZFS_LOG", logFile)

	svc, _ := newTestService(t)
	return svc, logFile
}

// fixtureProps — propiedades típicas de 'zfs get -H -o name,property,value,source all tank/docs'.
const fixtureProps = "tank/docs\tcompression\tlz4\tlocal\n" +
	"tank/docs\trecordsize\t128K\tlocal\n" +
	"tank/docs\tatime\ton\tdefault\n" +
	"tank/docs\tquota\tnone\tdefault\n" +
	"tank/docs\tmountpoint\t/mnt/docs\tlocal\n" +
	"tank/docs\texec\ton\tdefault\n" +
	"tank/docs\tencryption\toff\tdefault\n" +
	"tank/docs\tused\t1610612736\t-\n" +
	"tank/docs\tuser:backup\ttrue\tlocal\n"

func TestPropsGet(t *testing.T) {
	svc, _ := newPropsTestService(t)

	props, err := svc.DatasetPropsGet(context.Background(), "tank/docs")
	if err != nil {
		t.Fatalf("DatasetPropsGet: %v", err)
	}
	if len(props) != 9 {
		t.Fatalf("len(props) = %d, esperaba 9", len(props))
	}
	if props[0].Name != "compression" || props[0].Value != "lz4" || props[0].Source != "local" {
		t.Fatalf("props[0] = %+v", props[0])
	}
	// Orden preservado del fixture.
	if props[8].Name != "user:backup" {
		t.Fatalf("user prop no está al final: %s", props[8].Name)
	}
}

func TestPropsGetNombreInvalido(t *testing.T) {
	svc, _ := newPropsTestService(t)
	_, err := svc.DatasetPropsGet(context.Background(), "tank@docs")
	if !errors.Is(err, ErrInvalidName) {
		t.Fatalf("err = %v, esperaba ErrInvalidName", err)
	}
}

func TestPropValidators(t *testing.T) {
	cases := []struct {
		prop, val string
		ok        bool
	}{
		{"compression", "lz4", true},
		{"compression", "zstd", true},
		{"compression", "none", false}, // none no es compresión válida
		{"atime", "on", true},
		{"atime", "off", true},
		{"atime", "1", false},
		{"recordsize", "128K", true},
		{"recordsize", "64K", true},
		{"recordsize", "100K", false}, // no potencia de 2
		{"recordsize", "16M", true},
		{"recordsize", "32M", false}, // > 16M
		{"recordsize", "1T", false},  // > 16M
		{"sync", "always", true},
		{"sync", "on", false},
		{"quota", "none", true},
		{"quota", "1T", true},
		{"quota", "1.5T", false}, // no numérico
		{"quota", "500G", true},
		{"mountpoint", "/tank/docs", true},
		{"mountpoint", "none", true},
		{"mountpoint", "legacy", true},
		{"mountpoint", "../etc/passwd", false},
		{"mountpoint", "/tmp/x; rm -rf /", false},
		{"volblocksize", "512", true},
		{"volblocksize", "128K", true},
		{"volblocksize", "64M", false}, // > 128K
		{"copies", "2", true},
		{"copies", "4", false},
	}
	for _, c := range cases {
		spec, ok := propValidators[c.prop]
		if !ok {
			t.Fatalf("propiedad %s no está en la whitelist", c.prop)
		}
		if got := spec.valid(c.val); got != c.ok {
			t.Errorf("propValidators[%s].valid(%q) = %v, esperaba %v", c.prop, c.val, got, c.ok)
		}
	}
}

func TestPropAppliesTo(t *testing.T) {
	if propValidators["mountpoint"].appliesTo("volume") {
		t.Error("mountpoint no debería aplicar a volume")
	}
	if !propValidators["mountpoint"].appliesTo("fs") {
		t.Error("mountpoint debería aplicar a fs")
	}
	if propValidators["volsize"].appliesTo("fs") {
		t.Error("volsize no debería aplicar a fs")
	}
	if !propValidators["volsize"].appliesTo("volume") {
		t.Error("volsize debería aplicar a volume")
	}
	if !propValidators["atime"].appliesTo("fs") || !propValidators["atime"].appliesTo("volume") {
		t.Error("atime debería aplicar a ambos tipos")
	}
}

func TestPropSetValido(t *testing.T) {
	svc, logFile := newPropsTestService(t)

	if err := svc.DatasetPropSet(context.Background(), "tester", "tank/docs", "recordsize", "64K", "fs", false); err != nil {
		t.Fatalf("DatasetPropSet: %v", err)
	}
	out, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("el fake zfs no registró la llamada: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "recordsize=64K tank/docs" {
		t.Fatalf("args de zfs = %q, esperaba %q", got, "recordsize=64K tank/docs")
	}
}

func TestPropSetInvalido(t *testing.T) {
	svc, logFile := newPropsTestService(t)

	// Valor no válido → no debe llegar a zfs.
	if err := svc.DatasetPropSet(context.Background(), "tester", "tank/docs", "recordsize", "100K", "fs", false); err == nil {
		t.Fatal("set de recordsize=100K debería fallar")
	}
	// Propiedad fuera de whitelist.
	if err := svc.DatasetPropSet(context.Background(), "tester", "tank/docs", "dedup", "on", "fs", false); err == nil {
		t.Fatal("set de dedup debería fallar (fuera de whitelist)")
	}
	// Propiedad no aplicable al tipo.
	if err := svc.DatasetPropSet(context.Background(), "tester", "tank/docs", "mountpoint", "/x", "volume", false); err == nil {
		t.Fatal("set de mountpoint en volume debería fallar")
	}
	if b, _ := os.ReadFile(logFile); len(b) != 0 {
		t.Fatalf("zfs no debería haberse llamado para sets inválidos: %q", b)
	}
}

func TestPropInherit(t *testing.T) {
	svc, logFile := newPropsTestService(t)

	if err := svc.DatasetPropInherit(context.Background(), "tester", "tank/docs", "recordsize", false); err != nil {
		t.Fatalf("DatasetPropInherit: %v", err)
	}
	out, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("el fake zfs no registró la llamada: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "recordsize tank/docs" {
		t.Fatalf("args de zfs = %q, esperaba %q", got, "recordsize tank/docs")
	}
}

func TestPropInheritInvalida(t *testing.T) {
	svc, logFile := newPropsTestService(t)
	if err := svc.DatasetPropInherit(context.Background(), "tester", "tank/docs", "dedup", false); err == nil {
		t.Fatal("inherit de dedup debería fallar (fuera de whitelist)")
	}
	if b, _ := os.ReadFile(logFile); len(b) != 0 {
		t.Fatalf("zfs no debería haberse llamado: %q", b)
	}
}

// §6: values that disable corruption detection are not offered at all.
func TestChecksumOffAndFletcher2Refused(t *testing.T) {
	spec := propValidators["checksum"]
	for _, v := range []string{"off", "fletcher2"} {
		if spec.valid(v) {
			t.Errorf("checksum=%s accepted", v)
		}
	}
	for _, v := range []string{"on", "fletcher4", "sha256"} {
		if !spec.valid(v) {
			t.Errorf("checksum=%s refused", v)
		}
	}
}

// newRiskService — a fake zfs that reports a 10 GiB volume for
// 'get -Hp -o value volsize' and logs every other call.
func newRiskService(t *testing.T) (*Service, string) {
	t.Helper()
	svc, logFile := newTestService(t)
	dir := t.TempDir()
	zfs := "#!/bin/sh\n" +
		"if [ \"$1\" = get ] && [ \"$5\" = volsize ]; then echo 10737418240; exit 0; fi\n" +
		"echo \"$@\" >> " + logFile + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "zfs"), []byte(zfs), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return svc, logFile
}

func TestHighImpactPropsNeedAcknowledgement(t *testing.T) {
	ctx := context.Background()
	svc, logFile := newRiskService(t)
	if err := svc.DatasetPropSet(ctx, "tester", "tank/docs", "sync", "disabled", "fs", false); !errors.Is(err, ErrRiskAck) {
		t.Fatalf("sync=disabled without acknowledgement: %v, want ErrRiskAck", err)
	}
	if out, _ := os.ReadFile(logFile); len(out) != 0 {
		t.Fatalf("zfs ran without acknowledgement: %s", out)
	}
	if err := svc.DatasetPropSet(ctx, "tester", "tank/docs", "sync", "disabled", "fs", true); err != nil {
		t.Fatalf("sync=disabled with acknowledgement: %v", err)
	}
	if err := svc.DatasetPropSet(ctx, "tester", "tank/docs", "sync", "standard", "fs", false); err != nil {
		t.Fatalf("sync=standard needs no acknowledgement: %v", err)
	}
	if err := svc.DatasetPropInherit(ctx, "tester", "tank/docs", "sync", false); !errors.Is(err, ErrRiskAck) {
		t.Fatalf("inheriting sync without acknowledgement: %v, want ErrRiskAck", err)
	}
}

// Shrinking a volume destroys data past the new end; growing it does not.
func TestVolsizeShrinkNeedsAcknowledgement(t *testing.T) {
	ctx := context.Background()
	svc, _ := newRiskService(t)
	if err := svc.DatasetPropSet(ctx, "tester", "tank/vol", "volsize", "5G", "volume", false); !errors.Is(err, ErrRiskAck) {
		t.Fatalf("shrink 10G->5G without acknowledgement: %v, want ErrRiskAck", err)
	}
	if err := svc.DatasetPropSet(ctx, "tester", "tank/vol", "volsize", "20G", "volume", false); err != nil {
		t.Fatalf("grow 10G->20G needs no acknowledgement: %v", err)
	}
}
