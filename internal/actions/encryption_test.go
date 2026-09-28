// encryption_test.go — cifrado nativo por dataset (lote D): create cifrado,
// load/unload/change-key con fake zfs que registra argv y stdin por separado.
// Regla de oro verificada aquí: la passphrase viaja SOLO por stdin, JAMÁS en
// argv (visible en ps) ni en audit_log.
package actions

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"easyzfs/internal/db"
)

// newKeyTestService — como newTestService pero el fake 'zfs' anota argv en
// zfs-args.log y lo que reciba por stdin en zfs-stdin.log.
func newKeyTestService(t *testing.T) (*Service, string, string) {
	t.Helper()
	dir := t.TempDir()
	argsLog := filepath.Join(dir, "zfs-args.log")
	stdinLog := filepath.Join(dir, "zfs-stdin.log")

	zfs := "#!/bin/sh\necho \"$@\" >> " + argsLog + "\ncat >> " + stdinLog + "\nexit 0\n"
	zpool := "#!/bin/sh\nexit 0\n"
	sudo := "#!/bin/sh\nwhile [ $# -gt 0 ]; do case \"$1\" in -*) shift;; *) break;; esac; done\nexec \"$@\"\n"
	for name, body := range map[string]string{"zfs": zfs, "zpool": zpool, "sudo": sudo} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	stubPoolRoot(t, "")
	stubDefaultMountpoints(t)

	d, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := db.Migrate(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	return NewService(d), argsLog, stdinLog
}

const testPass = "cl4ve-sup3r-s3creta"

func TestDatasetCreateCifrado(t *testing.T) {
	svc, argsLog, stdinLog := newKeyTestService(t)

	err := svc.DatasetCreate(context.Background(), "tester", "tank", "secretos",
		"fs", "lz4", 0, 0, true, testPass, "")
	if err != nil {
		t.Fatalf("DatasetCreate cifrado: %v", err)
	}

	args, _ := os.ReadFile(argsLog)
	got := strings.TrimSpace(string(args))
	want := "create -p -o compression=lz4 -o encryption=aes-256-gcm -o keyformat=passphrase -o keylocation=prompt tank/secretos"
	if got != want {
		t.Fatalf("argv = %q, esperaba %q", got, want)
	}
	// La clave NUNCA en argv…
	if strings.Contains(got, testPass) {
		t.Fatalf("la passphrase aparece en argv: %q", got)
	}
	// …y SÍ por stdin (dos veces: verificación de zfs).
	stdin, _ := os.ReadFile(stdinLog)
	if got := string(stdin); got != testPass+"\n"+testPass+"\n" {
		t.Fatalf("stdin = %q, esperaba la passphrase dos veces", got)
	}
	// Audit: registra encrypted=true pero NUNCA la clave.
	var detail string
	if err := svc.db.QueryRow(
		"SELECT detail FROM audit_log WHERE action='dataset.create' AND target='tank/secretos'").Scan(&detail); err != nil {
		t.Fatalf("audit_log: %v", err)
	}
	if !strings.Contains(detail, `"encrypted":true`) {
		t.Errorf("audit detail sin encrypted: %s", detail)
	}
	if strings.Contains(detail, testPass) {
		t.Errorf("la passphrase aparece en audit_log: %s", detail)
	}
}

func TestDatasetCreateSinCifrarNoUsaStdin(t *testing.T) {
	svc, argsLog, stdinLog := newKeyTestService(t)
	if err := svc.DatasetCreate(context.Background(), "tester", "tank", "docs",
		"fs", "zstd", 0, 0, false, "", ""); err != nil {
		t.Fatalf("DatasetCreate: %v", err)
	}
	args, _ := os.ReadFile(argsLog)
	if strings.Contains(string(args), "encryption") {
		t.Errorf("argv con encryption sin pedirlo: %q", args)
	}
	stdin, _ := os.ReadFile(stdinLog)
	if len(stdin) != 0 {
		t.Errorf("stdin no vacío sin cifrado: %q", stdin)
	}
}

func TestDatasetCreatePassphraseCorta(t *testing.T) {
	svc, _, _ := newKeyTestService(t)
	err := svc.DatasetCreate(context.Background(), "tester", "tank", "x",
		"fs", "lz4", 0, 0, true, "corta", "")
	if err == nil {
		t.Fatal("passphrase <8 aceptada")
	}
}

func TestDatasetLoadKey(t *testing.T) {
	svc, argsLog, stdinLog := newKeyTestService(t)
	if err := svc.DatasetLoadKey(context.Background(), "tester", "tank/boveda", testPass); err != nil {
		t.Fatalf("DatasetLoadKey: %v", err)
	}
	args, _ := os.ReadFile(argsLog)
	if got := strings.TrimSpace(string(args)); got != "load-key tank/boveda" {
		t.Fatalf("argv = %q, esperaba 'load-key tank/boveda'", got)
	}
	if strings.Contains(string(args), testPass) {
		t.Fatal("la passphrase aparece en argv")
	}
	stdin, _ := os.ReadFile(stdinLog)
	if got := string(stdin); got != testPass+"\n" {
		t.Fatalf("stdin = %q, esperaba la passphrase", got)
	}
	// audit sin la clave
	var detail string
	if err := svc.db.QueryRow(
		"SELECT detail FROM audit_log WHERE action='dataset.unlock'").Scan(&detail); err != nil {
		t.Fatalf("audit_log: %v", err)
	}
	if strings.Contains(detail, testPass) {
		t.Errorf("la passphrase aparece en audit_log: %s", detail)
	}
}

func TestDatasetUnloadKey(t *testing.T) {
	svc, argsLog, stdinLog := newKeyTestService(t)
	if err := svc.DatasetUnloadKey(context.Background(), "tester", "tank/secretos"); err != nil {
		t.Fatalf("DatasetUnloadKey: %v", err)
	}
	args, _ := os.ReadFile(argsLog)
	// Sin -f por defecto (decisión: el error de zfs se muestra, no se fuerza).
	if got := strings.TrimSpace(string(args)); got != "unload-key tank/secretos" {
		t.Fatalf("argv = %q, esperaba 'unload-key tank/secretos'", got)
	}
	stdin, _ := os.ReadFile(stdinLog)
	if len(stdin) != 0 {
		t.Errorf("stdin no vacío en unload-key: %q", stdin)
	}
}

// newChangeKeyService — a fake zfs that models the three calls change-key
// makes: 'get encryptionroot', the dry-run 'load-key -n -L prompt' (which
// accepts only $ZFS_CURRENT_KEY on stdin, and otherwise fails with zfs's own
// wording), and 'change-key'. Every call logs its argv to zfs-args.log and
// its stdin to zfs-stdin.log, so the tests can check nothing leaks into argv
// and that change-key never runs after a failed check.
func newChangeKeyService(t *testing.T, currentKey, loadKeyFailure string) (*Service, string, string) {
	t.Helper()
	svc, argsLog, stdinLog := newKeyTestService(t)
	dir := t.TempDir()
	zfs := "#!/bin/sh\n" +
		"echo \"$@\" >> " + argsLog + "\n" +
		"case \"$1\" in\n" +
		"  get) echo tank/secretos ; exit 0 ;;\n" +
		"  load-key)\n" +
		"    IFS= read -r k ; printf '%s\\n' \"$k\" >> " + stdinLog + "\n" +
		"    if [ -n \"$ZFS_LOAD_KEY_FAILURE\" ]; then echo \"$ZFS_LOAD_KEY_FAILURE\" >&2 ; exit 1 ; fi\n" +
		"    if [ \"$k\" = \"$ZFS_CURRENT_KEY\" ]; then exit 0 ; fi\n" +
		"    echo \"Key load error: Incorrect key provided for 'tank/secretos'.\" >&2 ; exit 255 ;;\n" +
		"  change-key) cat >> " + stdinLog + " ; exit 0 ;;\n" +
		"esac\n" +
		"exit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "zfs"), []byte(zfs), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ZFS_CURRENT_KEY", currentKey)
	t.Setenv("ZFS_LOAD_KEY_FAILURE", loadKeyFailure)
	return svc, argsLog, stdinLog
}

func TestDatasetChangeKey(t *testing.T) {
	actual := "v13ja-cl4ve-larga"
	nueva := "nu3va-cl4ve-larga"
	svc, argsLog, stdinLog := newChangeKeyService(t, actual, "")
	if err := svc.DatasetChangeKey(context.Background(), "tester", "tank/secretos", actual, nueva); err != nil {
		t.Fatalf("DatasetChangeKey: %v", err)
	}
	args, _ := os.ReadFile(argsLog)
	want := "get -H -o value encryptionroot tank/secretos\n" +
		"load-key -n -L prompt tank/secretos\n" +
		"change-key -o keyformat=passphrase tank/secretos\n"
	if string(args) != want {
		t.Fatalf("argv =\n%s\nwant\n%s", args, want)
	}
	if strings.Contains(string(args), nueva) || strings.Contains(string(args), actual) {
		t.Fatal("a passphrase appears in argv")
	}
	stdin, _ := os.ReadFile(stdinLog)
	if s := string(stdin); s != actual+"\n"+nueva+"\n"+nueva+"\n" {
		t.Fatalf("stdin = %q, want the current key once, then the new one twice", s)
	}
	var detail string
	if err := svc.db.QueryRow(
		"SELECT detail FROM audit_log WHERE action='dataset.change_key'").Scan(&detail); err != nil {
		t.Fatalf("audit_log: %v", err)
	}
	if strings.Contains(detail, nueva) || strings.Contains(detail, actual) {
		t.Errorf("a passphrase appears in audit_log: %s", detail)
	}
}

// Any non-empty current_key used to pass. A wrong one must now be refused
// before change-key runs, and the refusal audited without the key.
func TestDatasetChangeKeyWrongCurrentKey(t *testing.T) {
	svc, argsLog, _ := newChangeKeyService(t, "v13ja-cl4ve-larga", "")
	err := svc.DatasetChangeKey(context.Background(), "tester", "tank/secretos", "not-the-key", "nu3va-cl4ve-larga")
	if !errors.Is(err, ErrWrongKey) {
		t.Fatalf("err = %v, want ErrWrongKey", err)
	}
	args, _ := os.ReadFile(argsLog)
	if strings.Contains(string(args), "change-key") {
		t.Fatalf("change-key ran after a failed check:\n%s", args)
	}
	var n int
	svc.db.QueryRow("SELECT COUNT(*) FROM audit_log WHERE action='dataset.change_key'").Scan(&n)
	if n != 0 {
		t.Fatal("a refused change was audited as a successful one")
	}
	var detail string
	if err := svc.db.QueryRow(
		"SELECT detail FROM audit_log WHERE action='dataset.change_key.denied'").Scan(&detail); err != nil {
		t.Fatalf("the refusal was not audited: %v", err)
	}
	if strings.Contains(detail, "not-the-key") {
		t.Errorf("the attempted key appears in audit_log: %s", detail)
	}
}

// A failure that is not zfs saying the key is wrong must still refuse the
// change, but must not be reported as a wrong passphrase.
func TestDatasetChangeKeyCheckFailsClosed(t *testing.T) {
	svc, argsLog, _ := newChangeKeyService(t, "v13ja-cl4ve-larga", "cannot open 'tank/secretos': I/O error")
	err := svc.DatasetChangeKey(context.Background(), "tester", "tank/secretos", "v13ja-cl4ve-larga", "nu3va-cl4ve-larga")
	if err == nil {
		t.Fatal("the change went ahead although the key check failed")
	}
	if errors.Is(err, ErrWrongKey) {
		t.Fatalf("an I/O error was reported as a wrong key: %v", err)
	}
	args, _ := os.ReadFile(argsLog)
	if strings.Contains(string(args), "change-key") {
		t.Fatalf("change-key ran after a failed check:\n%s", args)
	}
}

func TestDatasetChangeKeyRequiresCurrent(t *testing.T) {
	svc, argsLog, _ := newChangeKeyService(t, "v13ja-cl4ve-larga", "")
	err := svc.DatasetChangeKey(context.Background(), "tester", "tank/secretos", "", "nu3va-cl4ve-larga")
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("err = %v, want ErrInvalidInput", err)
	}
	if args, _ := os.ReadFile(argsLog); len(args) != 0 {
		t.Fatalf("zfs was called without a current key:\n%s", args)
	}
}

func TestPoolExpand(t *testing.T) {
	svc, logFile := newTestService(t)
	if err := svc.PoolExpand(context.Background(), "tester", "tank", "raidz2-0", "sde", true); err != nil {
		t.Fatalf("PoolExpand: %v", err)
	}
	out, _ := os.ReadFile(logFile)
	if got := strings.TrimSpace(string(out)); got != "attach tank raidz2-0 sde" {
		t.Fatalf("argv = %q, esperaba 'attach tank raidz2-0 sde'", got)
	}
	// audit pool.expand con confirmed=1
	var confirmed int
	if err := svc.db.QueryRow(
		"SELECT confirmed FROM audit_log WHERE action='pool.expand' AND target='tank'").Scan(&confirmed); err != nil {
		t.Fatalf("audit_log: %v", err)
	}
	if confirmed != 1 {
		t.Errorf("confirmed=%d, esperaba 1", confirmed)
	}
	// vdev no raidz → inválido
	for _, bad := range []string{"mirror-0", "sdb", "raidz4-0", "raidz2", "raidz2-0;rm"} {
		if err := svc.PoolExpand(context.Background(), "tester", "tank", bad, "sde", true); err == nil {
			t.Errorf("PoolExpand vdev=%q aceptado", bad)
		}
	}
}

func TestDatasetCreateAtime(t *testing.T) {
	svc, argsLog, _ := newKeyTestService(t)

	if err := svc.DatasetCreate(context.Background(), "tester", "tank", "docs",
		"fs", "lz4", 0, 0, false, "", "relatime"); err != nil {
		t.Fatalf("DatasetCreate con atime: %v", err)
	}
	args, _ := os.ReadFile(argsLog)
	if got := strings.TrimSpace(string(args)); got != "create -p -o compression=lz4 -o atime=relatime tank/docs" {
		t.Fatalf("argv = %q, esperaba %q", got, "create -p -o compression=lz4 -o atime=relatime tank/docs")
	}
}

func TestDatasetCreateAtimeVacioNoTocaAtime(t *testing.T) {
	svc, argsLog, _ := newKeyTestService(t)

	if err := svc.DatasetCreate(context.Background(), "tester", "tank", "docs",
		"fs", "lz4", 0, 0, false, "", ""); err != nil {
		t.Fatalf("DatasetCreate: %v", err)
	}
	args, _ := os.ReadFile(argsLog)
	if got := strings.TrimSpace(string(args)); got != "create -p -o compression=lz4 tank/docs" {
		t.Fatalf("argv = %q, esperaba %q", got, "create -p -o compression=lz4 tank/docs")
	}
}

func TestDatasetCreateAtimeInvalido(t *testing.T) {
	svc, _, _ := newKeyTestService(t)
	if err := svc.DatasetCreate(context.Background(), "tester", "tank", "docs",
		"fs", "lz4", 0, 0, false, "", "atime=on;rm -rf"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("atime inválido = %v, esperaba ErrInvalidInput", err)
	}
}
