//go:build integration && linux

// Интеграционные тесты: запускаются внутри Linux-контейнера, где есть
// настоящие MariaDB, PostgreSQL, nginx, sshd и файловая система с
// владельцами и правами.
//
// Здесь проверяется ровно то, чего не проверить на Windows: дампы баз,
// сохранение владельцев и xattr, работа через настоящий sshd с ключом,
// ограниченным command=, и восстановление базы поверх боевой.
//
// Бинарь собирается на хосте (GOOS=linux go test -c) и копируется в образ,
// поэтому компилятор Go в контейнере не нужен.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/iamtime/autobak/internal/app"
	"github.com/iamtime/autobak/internal/backend"
	"github.com/iamtime/autobak/internal/collect"
	"github.com/iamtime/autobak/internal/discover"
	"github.com/iamtime/autobak/internal/engine"
	"github.com/iamtime/autobak/internal/plan"
	"github.com/iamtime/autobak/internal/repo"
	"github.com/iamtime/autobak/internal/restore"
	"github.com/iamtime/autobak/internal/sshx"
	"golang.org/x/sys/unix"
)

const agentPath = "/usr/local/bin/autobak-agent"

func newRepo(t *testing.T) *repo.Repo {
	t.Helper()
	be, err := backend.OpenLocal(t.TempDir(), backend.Caps{CanWrite: true, CanDelete: true})
	if err != nil {
		t.Fatal(err)
	}
	r, _, err := repo.Init(context.Background(), be, "пароль-репозитория", "интеграция")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func run(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

func mysql(t *testing.T, sql string) string {
	t.Helper()
	return run(t, "mariadb", "--default-character-set=utf8mb4",
		"--batch", "--skip-column-names", "-e", sql)
}

func psql(t *testing.T, sql string) string {
	t.Helper()
	return run(t, "su", "-s", "/bin/sh", "postgres", "-c",
		"psql -At -c "+shellQuote(sql))
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// snapshotFiles собирает содержимое снимка в карту «путь → содержимое».
func snapshotFiles(t *testing.T, r *repo.Repo, snap *repo.Snapshot) map[string]string {
	t.Helper()
	ctx := context.Background()
	out := map[string]string{}
	err := r.ReadTree(ctx, snap, func(n *repo.Node) error {
		if n.Type != repo.NodeFile {
			return nil
		}
		var buf bytes.Buffer
		if _, err := r.ReadStream(ctx, n.Chunks, &buf); err != nil {
			return err
		}
		out[n.Path] = buf.String()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func snapshotNodes(t *testing.T, r *repo.Repo, snap *repo.Snapshot) map[string]repo.Node {
	t.Helper()
	out := map[string]repo.Node{}
	if err := r.ReadTree(context.Background(), snap, func(n *repo.Node) error {
		out[n.Path] = *n
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// --- Файлы: владельцы, права, ссылки, xattr -------------------------------

func TestFilesPreserveOwnershipAndAttrs(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	// Разложим то, что обычно и ломается при восстановлении.
	mustWrite(t, filepath.Join(root, "index.php"), "<?php echo 1;", 0o644)
	mustWrite(t, filepath.Join(root, "secret.key"), "ключ", 0o600)
	if err := os.MkdirAll(filepath.Join(root, "storage"), 0o2775); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("index.php", filepath.Join(root, "current.php")); err != nil {
		t.Fatal(err)
	}
	// Владелец www-data: типичный владелец файлов сайта.
	wwwUID, wwwGID := lookupUser(t, "www-data")
	for _, p := range []string{root, filepath.Join(root, "index.php"), filepath.Join(root, "storage")} {
		if err := os.Chown(p, wwwUID, wwwGID); err != nil {
			t.Fatal(err)
		}
	}
	// setgid на каталоге: без него у всего, что создаётся внутри,
	// окажется не та группа.
	if err := os.Chmod(filepath.Join(root, "storage"), os.ModeSetgid|0o775); err != nil {
		t.Fatal(err)
	}
	if err := unix.Lsetxattr(filepath.Join(root, "index.php"),
		"user.autobak.test", []byte("значение"), 0); err != nil {
		t.Logf("xattr недоступны в этой ФС: %v", err)
	}

	r := newRepo(t)
	p := plan.New("box")
	p.Modules = []plan.Module{{
		Kind: plan.KindFiles, Name: "site", Enabled: true, Paths: []string{root},
	}}
	snap, err := engine.Backup(ctx, r, engine.Options{Plan: p, Server: "box", Agent: "test"})
	if err != nil {
		t.Fatal(err)
	}

	nodes := snapshotNodes(t, r, snap)
	idx := nodes[filepath.Join(root, "index.php")]
	if idx.User != "www-data" || idx.UID != uint32(wwwUID) {
		t.Fatalf("владелец файла не сохранён: %+v", idx)
	}
	if idx.Mode != 0o644 {
		t.Fatalf("права файла: %o", idx.Mode)
	}
	if nodes[filepath.Join(root, "secret.key")].Mode != 0o600 {
		t.Fatal("права 600 не сохранены")
	}
	st := nodes[filepath.Join(root, "storage")]
	if st.Mode&0o2000 == 0 {
		t.Fatalf("бит setgid потерян: %o", st.Mode)
	}
	link := nodes[filepath.Join(root, "current.php")]
	if link.Type != repo.NodeSymlink || link.Link != "index.php" {
		t.Fatalf("символическая ссылка не сохранена: %+v", link)
	}
	if v := idx.XAttrs["user.autobak.test"]; v != "" && v != "значение" {
		t.Fatalf("xattr искажён: %q", v)
	}

	// Восстановление на место с возвратом владельцев.
	dst := t.TempDir()
	rep, err := restore.Run(ctx, r, snap, restore.Options{},
		restore.NewFS(restore.FSOptions{Root: dst, RestoreOwner: true}))
	if err != nil {
		t.Fatal(err)
	}
	t.Log(rep.Summary())

	base := filepath.Join(dst, strings.TrimPrefix(root, "/"))
	var stat syscall.Stat_t
	if err := syscall.Lstat(filepath.Join(base, "index.php"), &stat); err != nil {
		t.Fatal(err)
	}
	if int(stat.Uid) != wwwUID || int(stat.Gid) != wwwGID {
		t.Fatalf("владелец не восстановлен: uid=%d gid=%d, ожидалось %d/%d",
			stat.Uid, stat.Gid, wwwUID, wwwGID)
	}
	if stat.Mode&0o777 != 0o644 {
		t.Fatalf("права не восстановлены: %o", stat.Mode&0o777)
	}
	var sst syscall.Stat_t
	if err := syscall.Lstat(filepath.Join(base, "storage"), &sst); err != nil {
		t.Fatal(err)
	}
	if sst.Mode&syscall.S_ISGID == 0 {
		t.Fatalf("setgid не восстановлен: %o", sst.Mode)
	}
	target, err := os.Readlink(filepath.Join(base, "current.php"))
	if err != nil || target != "index.php" {
		t.Fatalf("ссылка не восстановлена: %q, %v", target, err)
	}
	if v, err := getxattr(filepath.Join(base, "index.php"), "user.autobak.test"); err == nil && v != "значение" {
		t.Fatalf("xattr не восстановлен: %q", v)
	}
}

// --- MySQL ----------------------------------------------------------------

func TestMySQLBackupAndRestore(t *testing.T) {
	ctx := context.Background()

	mysql(t, "DROP DATABASE IF EXISTS shop_test")
	mysql(t, "CREATE DATABASE shop_test CHARACTER SET utf8mb4")
	mysql(t, `USE shop_test;
		CREATE TABLE orders (
			id INT AUTO_INCREMENT PRIMARY KEY,
			customer VARCHAR(100) NOT NULL,
			total DECIMAL(10,2),
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			KEY idx_customer (customer)
		) ENGINE=InnoDB;
		CREATE TRIGGER orders_guard BEFORE INSERT ON orders
			FOR EACH ROW SET NEW.total = IFNULL(NEW.total, 0);`)
	// Одним запросом, а не пятьюстами: каждый вызов mariadb - это новый
	// процесс, и в сумме они дали бы полминуты на пустом месте.
	var vals []string
	for i := range 500 {
		vals = append(vals, fmt.Sprintf("('покупатель №%d', %d.50)", i, i*7))
	}
	mysql(t, "INSERT INTO shop_test.orders (customer, total) VALUES "+strings.Join(vals, ","))
	before := strings.TrimSpace(mysql(t, "SELECT COUNT(*), SUM(total) FROM shop_test.orders"))
	t.Logf("в базе до бэкапа: %s", before)

	r := newRepo(t)
	p := plan.New("box")
	p.Modules = []plan.Module{{
		Kind: plan.KindMySQL, Name: "MySQL", Enabled: true, Databases: []string{"shop_test"},
	}}
	snap, err := engine.Backup(ctx, r, engine.Options{Plan: p, Server: "box", Agent: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Complete() {
		t.Fatalf("модуль MySQL отработал с ошибкой: %+v", snap.Failed())
	}

	files := snapshotFiles(t, r, snap)
	dump, ok := files[collect.VirtualMySQL+"/shop_test.sql"]
	if !ok {
		t.Fatalf("дампа базы нет в снимке, есть: %v", keys(files))
	}
	for _, must := range []string{
		"CREATE TABLE `orders`",
		"idx_customer",
		"покупатель №499",
		"orders_guard", // триггеры обязаны попасть в дамп
	} {
		if !strings.Contains(dump, must) {
			t.Errorf("в дампе нет %q", must)
		}
	}
	if _, ok := files[collect.VirtualMySQL+"/@grants.sql"]; !ok {
		t.Error("права пользователей не сохранены")
	}
	t.Logf("дамп %d КБ, в хранилище %s",
		len(dump)/1024, repo.HumanBytes(snap.Stats.BytesStored))

	// Теперь самое важное: уничтожаем базу и возвращаем её из снимка.
	mysql(t, "DROP DATABASE shop_test")
	if out := mysql(t, "SHOW DATABASES"); strings.Contains(out, "shop_test") {
		t.Fatal("база не удалилась - тест бессмысленен")
	}

	dbOpts := restore.DBOptions{
		Mode: restore.DBRestore, InPlace: true,
		Log: func(l, m string) { t.Logf("[%s] %s", l, m) },
	}
	rep, err := restore.Run(ctx, r, snap, restore.Options{}, restore.NewFS(restore.FSOptions{
		Root:    t.TempDir(),
		Virtual: restore.NewDBHandler(ctx, dbOpts),
	}))
	if err != nil {
		t.Fatalf("восстановление базы не удалось: %v", err)
	}
	t.Log(rep.Summary())

	after := strings.TrimSpace(mysql(t, "SELECT COUNT(*), SUM(total) FROM shop_test.orders"))
	if after != before {
		t.Fatalf("данные после восстановления: %q, было %q", after, before)
	}
	if !strings.Contains(mysql(t, "SHOW TRIGGERS FROM shop_test"), "orders_guard") {
		t.Error("триггер не восстановился")
	}
	t.Logf("база восстановлена полностью: %s", after)
}

// Безопасный режим: база возвращается под другим именем, боевая не тронута.
func TestMySQLRestoreIntoSeparateDatabase(t *testing.T) {
	ctx := context.Background()

	mysql(t, "DROP DATABASE IF EXISTS safe_test; DROP DATABASE IF EXISTS safe_test_copy")
	mysql(t, "CREATE DATABASE safe_test")
	mysql(t, "CREATE TABLE safe_test.t (id INT, v VARCHAR(50))")
	mysql(t, "INSERT INTO safe_test.t VALUES (1, 'из снимка')")

	r := newRepo(t)
	p := plan.New("box")
	p.Modules = []plan.Module{{
		Kind: plan.KindMySQL, Name: "MySQL", Enabled: true, Databases: []string{"safe_test"},
	}}
	snap, err := engine.Backup(ctx, r, engine.Options{Plan: p, Server: "box", Agent: "test"})
	if err != nil {
		t.Fatal(err)
	}

	// Меняем боевую базу - она обязана остаться такой после восстановления.
	mysql(t, "UPDATE safe_test.t SET v = 'боевые данные'")

	dbOpts := restore.DBOptions{Mode: restore.DBRestore, Suffix: "_copy"}
	if _, err := restore.Run(ctx, r, snap, restore.Options{}, restore.NewFS(restore.FSOptions{
		Root: t.TempDir(), Virtual: restore.NewDBHandler(ctx, dbOpts),
	})); err != nil {
		t.Fatal(err)
	}

	live := strings.TrimSpace(mysql(t, "SELECT v FROM safe_test.t"))
	copyv := strings.TrimSpace(mysql(t, "SELECT v FROM safe_test_copy.t"))
	if live != "боевые данные" {
		t.Fatalf("боевая база пострадала: %q", live)
	}
	if copyv != "из снимка" {
		t.Fatalf("копия содержит %q вместо данных снимка", copyv)
	}
	t.Logf("боевая: %q, копия: %q - CREATE DATABASE и USE вырезаны верно", live, copyv)
}

// --- PostgreSQL -----------------------------------------------------------

func TestPostgresBackupAndRestore(t *testing.T) {
	ctx := context.Background()

	run(t, "su", "-s", "/bin/sh", "postgres", "-c", "dropdb --if-exists pg_test")
	run(t, "su", "-s", "/bin/sh", "postgres", "-c", "createdb pg_test")
	// psql по умолчанию работает с базой postgres - все запросы теста
	// идут через отдельный вызов с -d pg_test.
	inTest := func(sql string) string {
		return run(t, "su", "-s", "/bin/sh", "postgres", "-c",
			"psql -At -d pg_test -c "+shellQuote(sql))
	}
	inTest(`CREATE TABLE items (id serial primary key, name text, price numeric)`)
	inTest(`INSERT INTO items (name, price)
	        SELECT 'товар ' || i, i + 0.99 FROM generate_series(1, 200) AS i`)
	before := strings.TrimSpace(inTest("SELECT count(*), sum(price) FROM items"))

	r := newRepo(t)
	p := plan.New("box")
	p.Modules = []plan.Module{{
		Kind: plan.KindPostgres, Name: "PostgreSQL", Enabled: true, Databases: []string{"pg_test"},
	}}
	snap, err := engine.Backup(ctx, r, engine.Options{Plan: p, Server: "box", Agent: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Complete() {
		t.Fatalf("модуль PostgreSQL отработал с ошибкой: %+v", snap.Failed())
	}
	files := snapshotFiles(t, r, snap)
	if _, ok := files[collect.VirtualPostgres+"/pg_test.dump"]; !ok {
		t.Fatalf("дампа нет в снимке: %v", keys(files))
	}
	if _, ok := files[collect.VirtualPostgres+"/@globals.sql"]; !ok {
		t.Error("роли кластера не сохранены")
	}

	run(t, "su", "-s", "/bin/sh", "postgres", "-c", "dropdb pg_test")

	dbOpts := restore.DBOptions{Mode: restore.DBRestore, InPlace: true,
		Log: func(l, m string) { t.Logf("[%s] %s", l, m) }}
	if _, err := restore.Run(ctx, r, snap, restore.Options{}, restore.NewFS(restore.FSOptions{
		Root: t.TempDir(), Virtual: restore.NewDBHandler(ctx, dbOpts),
	})); err != nil {
		t.Fatalf("восстановление не удалось: %v", err)
	}
	after := strings.TrimSpace(inTest("SELECT count(*), sum(price) FROM items"))
	if after != before {
		t.Fatalf("после восстановления %q, было %q", after, before)
	}
	t.Logf("PostgreSQL восстановлен: %s", after)
}

// Дамп PostgreSQL берётся с -Z0 именно ради этого: сжатый самим pg_dump
// поток менялся бы целиком от одной изменённой строки.
//
// Проверяется по шагам, чтобы при падении было видно, что именно сломалось:
// воспроизводим ли дамп вообще, дедуплицируется ли неизменный, и во что
// обходится правка одной строки.
func TestPostgresDumpIsDeduplicable(t *testing.T) {
	ctx := context.Background()
	inTest := func(sql string) string {
		return run(t, "su", "-s", "/bin/sh", "postgres", "-c",
			"psql -At -d dedup_test -c "+shellQuote(sql))
	}
	run(t, "su", "-s", "/bin/sh", "postgres", "-c", "dropdb --if-exists dedup_test")
	run(t, "su", "-s", "/bin/sh", "postgres", "-c", "createdb dedup_test")
	inTest("CREATE TABLE big (id serial primary key, payload text)")
	// Содержимое строк разное, как в любой настоящей таблице, и объём
	// близок к дампу реального небольшого сайта. На вырожденных данных
	// (тысячи побайтово одинаковых строк) поиск границ по содержимому
	// вырождается в нарезку по счётчику байт, и мерить на них нечего.
	inTest(`INSERT INTO big (payload)
	        SELECT md5(i::text) || repeat(md5((i * 7)::text) || ' строка ' || i, 12)
	        FROM generate_series(1, 30000) AS i`)

	// Шаг 1: дамп обязан быть воспроизводимым.
	a := pgDump(t, "dedup_test")
	b := pgDump(t, "dedup_test")
	pre, suf := commonEdges(a, b)
	t.Logf("дамп %s, два подряд: совпадает начало %d, конец %d",
		repo.HumanBytes(int64(len(a))), pre, suf)
	if !bytes.Contains(a, []byte(" строка 1500")) {
		t.Fatal("данные в дампе сжаты - -Z0 не подействовал, дедупликации не будет")
	}
	if len(a)-pre-suf > len(a)/100 {
		t.Errorf("дамп невоспроизводим: одинаковая база даёт разный вывод")
	}

	r := newRepo(t)
	p := plan.New("box")
	p.Modules = []plan.Module{{
		Kind: plan.KindPostgres, Name: "PostgreSQL", Enabled: true, Databases: []string{"dedup_test"},
	}}

	// Шаг 2: повторный бэкап без изменений почти ничего не добавляет.
	first, err := engine.Backup(ctx, r, engine.Options{Plan: p, Server: "box", Agent: "test"})
	if err != nil {
		t.Fatal(err)
	}
	same, err := engine.Backup(ctx, r, engine.Options{Plan: p, Server: "box", Agent: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("бэкап: %s в %d чанках; повторный без изменений: %s в %d чанках",
		repo.HumanBytes(first.Stats.BytesTotal), first.Stats.ChunksTotal,
		repo.HumanBytes(same.Stats.BytesNew), same.Stats.ChunksNew)

	// Что здесь проверяется и почему пороги именно такие.
	//
	// Модуль снимает два файла: роли кластера и сам дамп базы. В начале
	// каждого стоит время снятия, поэтому два прогона подряд отличаются
	// первыми байтами всегда, и чанк, в который попал заголовок,
	// неизбежно оказывается новым. Таких чанков столько же, сколько
	// файлов, изредка на один больше - сдвиг границы в начале потока
	// может задеть следующий чанк.
	//
	// Размер этого чанка заранее не известен: граница по содержимому не
	// может встретиться раньше минимальных 128 КБ и обязана встретиться
	// к максимальным 2 МБ. Поэтому «почти ничего» здесь - это доля от
	// всего дампа, а не абсолютное число: порог в байтах, подогнанный
	// под удачный прогон, давал случайные падения при работе ровно так,
	// как задумано.
	const dumpedFiles = 2
	if same.Stats.ChunksNew > dumpedFiles+1 {
		t.Errorf("повторный бэкап неизменной базы создал %d новых чанков - ожидалось не больше %d, по одному на заголовок",
			same.Stats.ChunksNew, dumpedFiles+1)
	}
	if same.Stats.BytesNew > first.Stats.BytesTotal/10 {
		t.Errorf("повторный бэкап неизменной базы записал %s из %s - дедупликация не работает",
			repo.HumanBytes(same.Stats.BytesNew), repo.HumanBytes(first.Stats.BytesTotal))
	}

	// Шаг 3: правка одной строки. Она стоит примерно один чанк плюс тот,
	// в который попал заголовок архива со временем снятия дампа.
	inTest("UPDATE big SET payload = 'одна изменённая строка' WHERE id = 15000")
	third, err := engine.Backup(ctx, r, engine.Options{Plan: p, Server: "box", Agent: "test"})
	if err != nil {
		t.Fatal(err)
	}
	ratio := float64(third.Stats.BytesNew) / float64(first.Stats.BytesNew)
	t.Logf("после правки одной строки: %s, чанков новых %d из %d (%.1f%%)",
		repo.HumanBytes(third.Stats.BytesNew),
		third.Stats.ChunksNew, third.Stats.ChunksTotal, ratio*100)

	if third.Stats.ChunksNew > 4 {
		t.Fatalf("правка одной строки создала %d новых чанков - ожидалось не больше четырёх",
			third.Stats.ChunksNew)
	}
	if ratio > 0.15 {
		t.Fatalf("правка одной строки стоила %.0f%% дампа", ratio*100)
	}
}

func pgDump(t *testing.T, db string) []byte {
	t.Helper()
	out, err := exec.Command("su", "-s", "/bin/sh", "postgres", "-c",
		"pg_dump --format=custom -Z0 --no-password "+db).Output()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// commonEdges - длина совпадающего начала и совпадающего конца.
// По ним видно, отличаются ли потоки точечно или целиком.
func commonEdges(a, b []byte) (prefix, suffix int) {
	n := min(len(a), len(b))
	for prefix < n && a[prefix] == b[prefix] {
		prefix++
	}
	for suffix < n-prefix && a[len(a)-1-suffix] == b[len(b)-1-suffix] {
		suffix++
	}
	return prefix, suffix
}

// --- Автообнаружение ------------------------------------------------------

func TestDiscoveryFindsEverything(t *testing.T) {
	rep := discover.Run(context.Background(), "test")
	t.Logf("сводка: %s", rep.Summary())

	if rep.OS == "" || !strings.Contains(strings.ToLower(rep.OS), "debian") {
		t.Errorf("операционная система определена как %q", rep.OS)
	}
	if rep.Panel == nil || rep.Panel.Kind != "hestia" {
		t.Fatalf("панель не найдена: %+v", rep.Panel)
	}
	if len(rep.Sites) == 0 {
		t.Fatal("не найдено ни одного сайта")
	}
	var shop *discover.Site
	for i := range rep.Sites {
		if rep.Sites[i].Name == "shop.ru" {
			shop = &rep.Sites[i]
		}
	}
	if shop == nil {
		t.Fatalf("сайт shop.ru не найден среди %d найденных", len(rep.Sites))
	}
	if shop.User != "admin" || shop.PHP != "8.2" {
		t.Errorf("сайт разобран неверно: %+v", shop)
	}
	if shop.Size == 0 {
		t.Error("размер сайта не посчитан")
	}
	if len(shop.Databases) == 0 {
		t.Errorf("к сайту не привязана база: %+v", shop)
	}
	if rep.MySQL == nil || !rep.MySQL.Reachable {
		t.Fatalf("MySQL не опрошен: %+v", rep.MySQL)
	}
	if rep.Postgres == nil || !rep.Postgres.Reachable {
		t.Errorf("PostgreSQL не опрошен: %+v", rep.Postgres)
	}
	if len(rep.Configs) == 0 || !contains(rep.Configs, "/etc/nginx") {
		t.Errorf("конфигурации не найдены: %v", rep.Configs)
	}
	if len(rep.Mounts) == 0 {
		t.Error("разделы не определены")
	}

	// План, составленный по этой карте, обязан быть сразу рабочим.
	p := discover.Suggest(rep)
	if err := p.Validate(); err != nil {
		t.Fatalf("предложенный план непригоден: %v", err)
	}
	snap, err := engine.Backup(context.Background(), newRepo(t), engine.Options{
		Plan: p, Server: "box", Agent: "test",
		Log: func(l, m string) {
			if l != "info" {
				t.Logf("[%s] %s", l, m)
			}
		},
	})
	if err != nil {
		t.Fatalf("бэкап по автоматически составленному плану не прошёл: %v", err)
	}
	t.Logf("бэкап по найденному: файлов %d, %s, модулей %d",
		snap.Stats.Files, repo.HumanBytes(snap.Stats.BytesTotal), len(snap.Modules))
	for _, m := range snap.Modules {
		status := "ок"
		if !m.OK() {
			status = "ОШИБКА: " + m.Err
		}
		t.Logf("  %-28s файлов %-6d %-10s %s", m.Name, m.Files, repo.HumanBytes(m.Bytes), status)
	}
}

// --- Настоящий SSH --------------------------------------------------------

// Полный боевой путь: десктоп подключается по ssh ключом, ограниченным
// через command="autobak-agent serve", агент отдаёт поток, десктоп его
// шифрует и складывает. Плюс проверка, что этот ключ не даёт ничего сверх.
func TestBackupOverRealSSH(t *testing.T) {
	ctx := context.Background()
	keyPath := "/root/.ssh/autobak_test"
	if _, err := os.Stat(keyPath); err != nil {
		t.Skipf("ключ %s не подготовлен: %v", keyPath, err)
	}

	site := t.TempDir()
	mustWrite(t, filepath.Join(site, "index.php"), "<?php echo 'через ssh';", 0o644)
	mustWrite(t, filepath.Join(site, "data.bin"), strings.Repeat("данные", 20000), 0o644)

	a, err := app.OpenAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cr := &app.Repo{Name: "ssh-тест", Kind: app.RepoLocal, Path: t.TempDir()}
	if _, err := a.AddRepo(ctx, cr, "", "пароль-репозитория"); err != nil {
		t.Fatal(err)
	}
	s := &app.Server{
		Name: "box",
		SSH: sshx.Target{
			Host: "127.0.0.1", Port: 22, User: "root",
			KeyPath: keyPath, AgentPath: agentPath,
		},
		RepoID: cr.ID, Mode: app.ModePull,
	}
	if err := a.AddServer(s); err != nil {
		t.Fatal(err)
	}

	ver, err := s.SSH.Version(ctx)
	if err != nil {
		t.Fatalf("агент не отвечает по ssh: %v", err)
	}
	t.Logf("агент по ssh: %s", ver)

	s.Plan = *plan.New("box")
	s.Plan.Modules = []plan.Module{{
		Kind: plan.KindFiles, Name: "site", Enabled: true, Paths: []string{site},
	}}

	snap, err := a.Backup(ctx, s.ID, app.Events{
		Log: func(l, m string) {
			if l != "info" {
				t.Logf("[%s] %s", l, m)
			}
		},
	})
	if err != nil {
		t.Fatalf("бэкап через ssh не прошёл: %v", err)
	}
	if snap.Stats.Files != 2 {
		t.Fatalf("через ssh приехало %d файлов вместо 2", snap.Stats.Files)
	}
	t.Logf("через ssh: файлов %d, %s, в хранилище %s",
		snap.Stats.Files, repo.HumanBytes(snap.Stats.BytesTotal),
		repo.HumanBytes(snap.Stats.BytesStored))

	// И обратно - восстановление через тот же канал.
	dst := t.TempDir()
	rep, err := a.Restore(ctx, s.ID, app.RestoreOptions{
		SnapshotID: snap.ID, LocalDir: dst,
	}, app.Events{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dst, strings.TrimPrefix(site, "/"), "index.php"))
	if err != nil || string(got) != "<?php echo 'через ssh';" {
		t.Fatalf("файл не вернулся: %q, %v", got, err)
	}
	t.Logf("восстановление через ssh: %s", rep.Summary())
}

// Дедупликация по сети через настоящий sshd: второй бэкап не должен
// тянуть по каналу то, что уже лежит в хранилище. Ради этого агент
// получает ключ вычисления идентификаторов - и только его.
func TestWireDedupOverRealSSH(t *testing.T) {
	ctx := context.Background()
	keyPath := "/root/.ssh/autobak_test"
	if _, err := os.Stat(keyPath); err != nil {
		t.Skip("ключ не подготовлен")
	}

	site := t.TempDir()
	for i := range 12 {
		mustWrite(t, filepath.Join(site, fmt.Sprintf("file%02d.bin", i)),
			strings.Repeat(fmt.Sprintf("содержимое файла %d ", i), 30000), 0o644)
	}

	a, err := app.OpenAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cr := &app.Repo{Name: "wire", Kind: app.RepoLocal, Path: t.TempDir()}
	if _, err := a.AddRepo(ctx, cr, "", "пароль-репозитория"); err != nil {
		t.Fatal(err)
	}
	s := &app.Server{
		Name: "box",
		SSH: sshx.Target{Host: "127.0.0.1", Port: 22, User: "root",
			KeyPath: keyPath, AgentPath: agentPath},
		RepoID: cr.ID, Mode: app.ModePull,
	}
	if err := a.AddServer(s); err != nil {
		t.Fatal(err)
	}
	s.Plan = *plan.New("box")
	s.Plan.Modules = []plan.Module{{
		Kind: plan.KindFiles, Name: "site", Enabled: true, Paths: []string{site},
	}}

	first, err := a.Backup(ctx, s.ID, app.Events{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("первый бэкап: данных %s, по сети %s",
		repo.HumanBytes(first.Stats.BytesTotal), repo.HumanBytes(first.Stats.BytesWire))

	second, err := a.Backup(ctx, s.ID, app.Events{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("повторный без изменений: по сети %s из %s",
		repo.HumanBytes(second.Stats.BytesWire), repo.HumanBytes(first.Stats.BytesWire))

	if second.Stats.BytesWire > first.Stats.BytesWire/20 {
		t.Fatalf("по сети ушло %s - дедупликация через ssh не работает",
			repo.HumanBytes(second.Stats.BytesWire))
	}

	// Правка одного файла из двенадцати обязана стоить примерно этот файл.
	mustWrite(t, filepath.Join(site, "file05.bin"), "стало совсем коротко", 0o644)
	third, err := a.Backup(ctx, s.ID, app.Events{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("после правки одного файла из 12: по сети %s",
		repo.HumanBytes(third.Stats.BytesWire))
	if third.Stats.BytesWire > first.Stats.BytesWire/4 {
		t.Fatalf("правка одного файла стоила %s трафика",
			repo.HumanBytes(third.Stats.BytesWire))
	}

	// И снимок обязан остаться полноценным: восстановление сверяется с диском.
	dst := t.TempDir()
	if _, err := a.Restore(ctx, s.ID, app.RestoreOptions{
		SnapshotID: third.ID, LocalDir: dst,
	}, app.Events{}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dst, strings.TrimPrefix(site, "/"), "file05.bin"))
	if err != nil || string(got) != "стало совсем коротко" {
		t.Fatalf("изменённый файл восстановлен неверно: %q, %v", got, err)
	}
	got, err = os.ReadFile(filepath.Join(dst, strings.TrimPrefix(site, "/"), "file07.bin"))
	if err != nil || !strings.HasPrefix(string(got), "содержимое файла 7") {
		t.Fatalf("непереданный повторно файл восстановлен неверно: %v", err)
	}
}

// Ключ с command= обязан отказывать во всём, кроме белого списка команд.
// Это единственная преграда между украденным ключом и сервером.
func TestRestrictedKeyRefusesOtherCommands(t *testing.T) {
	keyPath := "/root/.ssh/autobak_test"
	if _, err := os.Stat(keyPath); err != nil {
		t.Skip("ключ не подготовлен")
	}
	sshArgs := []string{
		"-i", keyPath, "-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
		"root@127.0.0.1",
	}

	// Разрешённая команда обязана работать.
	if out, err := exec.Command("ssh", append(sshArgs, "autobak-agent version")...).CombinedOutput(); err != nil {
		t.Fatalf("разрешённая команда отвергнута: %v\n%s", err, out)
	}

	for _, forbidden := range []string{
		"cat /etc/shadow",
		"autobak-agent version; cat /etc/shadow",
		"autobak-agent serve",
		"/bin/sh",
		"autobak-agent version && id",
		"autobak-agent $(id)",
	} {
		out, err := exec.Command("ssh", append(sshArgs, forbidden)...).CombinedOutput()
		if err == nil {
			t.Errorf("команда %q была выполнена:\n%s", forbidden, out)
			continue
		}
		if strings.Contains(string(out), "root:") || strings.Contains(string(out), "uid=") {
			t.Errorf("команда %q утекла данными:\n%s", forbidden, out)
		}
		t.Logf("отклонено: %-42s → %s", forbidden, firstLine(string(out)))
	}
}

// --- Развёртывание на другой сервер ---------------------------------------

// Переезд целиком: снимок с одного сервера раскладывается на другой
// через настоящий sshd. Целью выступает тот же контейнер - для кода это
// полноценный второй сервер, до которого он идёт по сети.
func TestDeployToAnotherServer(t *testing.T) {
	ctx := context.Background()
	keyPath := "/root/.ssh/autobak_test"
	if _, err := os.Stat(keyPath); err != nil {
		t.Skip("ключ не подготовлен")
	}

	// Исходный сервер: сайт с файлами и правами.
	site := t.TempDir()
	mustWrite(t, filepath.Join(site, "index.php"), "<?php echo 'переехали';", 0o644)
	mustWrite(t, filepath.Join(site, "config", "app.ini"), "debug=0\nhost=old-server", 0o600)
	mustWrite(t, filepath.Join(site, "uploads", "logo.bin"),
		strings.Repeat("двоичные данные ", 20000), 0o644)

	a, err := app.OpenAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cr := &app.Repo{Name: "deploy", Kind: app.RepoLocal, Path: t.TempDir()}
	if _, err := a.AddRepo(ctx, cr, "", "пароль-репозитория"); err != nil {
		t.Fatal(err)
	}
	target := sshx.Target{Host: "127.0.0.1", Port: 22, User: "root",
		KeyPath: keyPath, AgentPath: agentPath}
	s := &app.Server{Name: "old", SSH: target, RepoID: cr.ID, Mode: app.ModePull}
	if err := a.AddServer(s); err != nil {
		t.Fatal(err)
	}
	s.Plan = *plan.New("old")
	s.Plan.Modules = []plan.Module{{
		Kind: plan.KindFiles, Name: "site.ru", Enabled: true, Paths: []string{site},
	}}
	snap, err := a.Backup(ctx, s.ID, app.Events{})
	if err != nil {
		t.Fatal(err)
	}

	opt := app.DefaultDeployOptions()
	opt.Source = s.ID
	opt.SnapshotID = snap.ID
	opt.Target = target
	// Только файлы: конфигурации системы на этом же контейнере трогать
	// нельзя, иначе развалятся остальные тесты.
	opt.Configs, opt.Databases, opt.Docker = false, false, false
	opt.Force = true

	// Сухой прогон обязан ничего не менять и всё показать.
	opt.DryRun = true
	dry, err := a.Deploy(ctx, opt, app.Events{})
	if err != nil {
		t.Fatalf("сухой прогон: %v", err)
	}
	if len(dry.Steps) == 0 {
		t.Fatal("сухой прогон не перечислил ни одного шага")
	}
	if len(dry.Checklist) == 0 {
		t.Fatal("не сказано, что осталось сделать вручную")
	}
	t.Logf("сухой прогон: %s", dry.Summary())
	for _, c := range dry.Checklist {
		t.Logf("  вручную: %s", c)
	}

	// Портим сайт на цели: развёртывание обязано вернуть его как было.
	mustWrite(t, filepath.Join(site, "index.php"), "СЛОМАНО", 0o644)
	if err := os.RemoveAll(filepath.Join(site, "uploads")); err != nil {
		t.Fatal(err)
	}

	opt.DryRun = false
	opt.Confirm = target.Label()
	rep, err := a.Deploy(ctx, opt, app.Events{
		Log: func(l, m string) {
			if l != "info" {
				t.Logf("[%s] %s", l, m)
			}
		},
	})
	if err != nil {
		t.Fatalf("развёртывание: %v", err)
	}
	for _, st := range rep.Steps {
		t.Logf("  %-32s %v %s", st.Name, st.OK, st.Detail+st.Err)
	}
	if !rep.OK() {
		t.Fatal("развёртывание завершилось с ошибками")
	}

	got, err := os.ReadFile(filepath.Join(site, "index.php"))
	if err != nil || string(got) != "<?php echo 'переехали';" {
		t.Fatalf("файл не восстановлен на цели: %q, %v", got, err)
	}
	got, err = os.ReadFile(filepath.Join(site, "uploads", "logo.bin"))
	if err != nil || len(got) != len(strings.Repeat("двоичные данные ", 20000)) {
		t.Fatalf("удалённый каталог не вернулся: %v", err)
	}
	// Права тоже: конфигурация с паролями не должна стать доступной всем.
	st, err := os.Stat(filepath.Join(site, "config", "app.ini"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("права после переезда: %o вместо 600", st.Mode().Perm())
	}
	t.Logf("развёрнуто: %s", rep.Summary())
}

// Развёртывание на непустой сервер обязано отказываться без явного
// разрешения: перезаписать работающий сервер необратимо.
func TestDeployRefusesNonEmptyTarget(t *testing.T) {
	ctx := context.Background()
	keyPath := "/root/.ssh/autobak_test"
	if _, err := os.Stat(keyPath); err != nil {
		t.Skip("ключ не подготовлен")
	}

	site := t.TempDir()
	mustWrite(t, filepath.Join(site, "x.txt"), "данные", 0o644)

	a, err := app.OpenAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cr := &app.Repo{Name: "deploy2", Kind: app.RepoLocal, Path: t.TempDir()}
	if _, err := a.AddRepo(ctx, cr, "", "пароль-репозитория"); err != nil {
		t.Fatal(err)
	}
	target := sshx.Target{Host: "127.0.0.1", Port: 22, User: "root",
		KeyPath: keyPath, AgentPath: agentPath}
	s := &app.Server{Name: "old2", SSH: target, RepoID: cr.ID, Mode: app.ModePull}
	if err := a.AddServer(s); err != nil {
		t.Fatal(err)
	}
	s.Plan = *plan.New("old2")
	s.Plan.Modules = []plan.Module{{
		Kind: plan.KindFiles, Name: "site", Enabled: true, Paths: []string{site},
	}}
	if _, err := a.Backup(ctx, s.ID, app.Events{}); err != nil {
		t.Fatal(err)
	}

	// На контейнере есть сайт shop.ru и базы - цель заведомо не пуста.
	opt := app.DefaultDeployOptions()
	opt.Source = s.ID
	opt.Target = target
	opt.Confirm = target.Label()
	opt.Force = false

	_, err = a.Deploy(ctx, opt, app.Events{})
	if err == nil {
		t.Fatal("развёртывание на непустой сервер прошло без разрешения")
	}
	if !strings.Contains(err.Error(), "уже есть данные") {
		t.Fatalf("отказ не про непустую цель: %v", err)
	}
	t.Logf("отклонено: %v", err)
}

// Ключ только для бэкапов не должен позволять восстановление.
//
// Это не придирка: команда восстановления пишет произвольные файлы от
// root, то есть ключ с её разрешением равносилен root - им можно
// положить свой authorized_keys и получить настоящий shell.
func TestBackupOnlyKeyRefusesRestore(t *testing.T) {
	keyPath := "/root/.ssh/autobak_test"
	if _, err := os.Stat(keyPath); err != nil {
		t.Skip("ключ не подготовлен")
	}
	// Отдельный ключ с ограничением только на бэкапы.
	boKey := "/root/.ssh/autobak_backup_only"
	if _, err := os.Stat(boKey); err != nil {
		run(t, "ssh-keygen", "-t", "ed25519", "-N", "", "-C", "backup-only", "-f", boKey)
		pub, err := os.ReadFile(boKey + ".pub")
		if err != nil {
			t.Fatal(err)
		}
		line := sshx.AuthorizedKeyLine(string(pub), agentPath, true, nil)
		f, err := os.OpenFile("/root/.ssh/authorized_keys", os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(line + "\n"); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}

	sshArgs := []string{
		"-i", boKey, "-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
		"root@127.0.0.1",
	}

	// Бэкапить этим ключом можно.
	if out, err := exec.Command("ssh", append(sshArgs, "autobak-agent version")...).CombinedOutput(); err != nil {
		t.Fatalf("бэкап-ключ не работает вовсе: %v\n%s", err, out)
	}
	if out, err := exec.Command("ssh", append(sshArgs, "autobak-agent discover --json")...).Output(); err != nil {
		t.Fatalf("обследование запрещено бэкап-ключу: %v", err)
	} else if len(out) < 10 {
		t.Fatal("обследование вернуло пустоту")
	}

	// А восстанавливать - нельзя.
	out, err := exec.Command("ssh", append(sshArgs, "autobak-agent import --root /tmp/x")...).CombinedOutput()
	if err == nil {
		t.Fatal("ключ только для бэкапов допустил восстановление")
	}
	if !strings.Contains(string(out), "только для бэкапов") {
		t.Fatalf("отказ не объясняет причину:\n%s", out)
	}
	t.Logf("отклонено: %s", firstLine(string(out)))
}

// --- Docker ---------------------------------------------------------------

func TestDockerCollector(t *testing.T) {
	if out, err := exec.Command("docker", "info").CombinedOutput(); err != nil {
		t.Skipf("docker недоступен в контейнере: %s", firstLine(string(out)))
	}
	ctx := context.Background()

	run(t, "docker", "volume", "create", "autobak-test-vol")
	t.Cleanup(func() { exec.Command("docker", "volume", "rm", "-f", "autobak-test-vol").Run() })

	// Кладём файл в том напрямую: драйвер local хранит данные на диске,
	// и именно оттуда их читает сборщик.
	volData := "/var/lib/docker/volumes/autobak-test-vol/_data"
	mustWrite(t, filepath.Join(volData, "dump.rdb"), "данные redis", 0o644)

	r := newRepo(t)
	p := plan.New("box")
	p.Modules = []plan.Module{{
		Kind: plan.KindDocker, Name: "Docker", Enabled: true,
		Volumes: []string{"autobak-test-vol"},
	}}
	snap, err := engine.Backup(ctx, r, engine.Options{Plan: p, Server: "box", Agent: "test",
		Log: func(l, m string) { t.Logf("[%s] %s", l, m) }})
	if err != nil {
		t.Fatal(err)
	}
	files := snapshotFiles(t, r, snap)
	want := collect.VirtualDocker + "/volumes/autobak-test-vol/dump.rdb"
	if files[want] != "данные redis" {
		t.Fatalf("содержимое тома не попало в снимок под %s, есть: %v", want, keys(files))
	}
	if _, ok := files[collect.VirtualDocker+"/inventory.json"]; !ok {
		t.Error("описание контейнеров не сохранено")
	}
	t.Logf("том сохранён пофайлово: %s", want)
}

// --- Обслуживание на живых данных ----------------------------------------

func TestVerifyAndPruneOnRealData(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	p := plan.New("box")
	p.Modules = []plan.Module{
		{Kind: plan.KindConfigs, Name: "Конфигурации", Enabled: true,
			Paths: []string{"/etc/nginx", "/etc/php"}},
	}
	for i := range 3 {
		if _, err := engine.Backup(ctx, r, engine.Options{
			Plan: p, Server: "box", Agent: "test",
		}); err != nil {
			t.Fatalf("бэкап %d: %v", i, err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	vr, err := r.Verify(ctx, repo.VerifyOptions{Sample: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !vr.OK() {
		t.Fatalf("проверка целостности не прошла: %v", vr.Problems)
	}
	t.Log(vr.Summary())

	opt := repo.DefaultPruneOptions()
	opt.Policy = repo.Retention{Last: 1}
	pr, err := r.Prune(ctx, opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(pr.Summary())

	snaps, err := r.ListSnapshots(ctx)
	if err != nil || len(snaps) != 1 {
		t.Fatalf("после очистки снимков: %d, %v", len(snaps), err)
	}
	vr2, err := r.Verify(ctx, repo.VerifyOptions{Sample: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !vr2.OK() {
		t.Fatalf("после очистки репозиторий повреждён: %v", vr2.Problems)
	}
	t.Logf("после очистки: %s", vr2.Summary())
}

// --- Агент как процесс ----------------------------------------------------

func TestAgentSelftestAndDiscover(t *testing.T) {
	out, err := exec.Command(agentPath, "selftest").CombinedOutput()
	t.Logf("selftest:\n%s", out)
	if err != nil {
		t.Errorf("selftest сообщил о проблемах: %v", err)
	}

	raw, err := exec.Command(agentPath, "discover", "--json").Output()
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	var rep discover.Report
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatalf("вывод discover не разбирается: %v", err)
	}
	if rep.Agent == "" || !rep.Root {
		t.Fatalf("агент сообщил: версия %q, root=%v", rep.Agent, rep.Root)
	}
}

// Конфигурация с ключами от хранилища, доступная посторонним, - это
// доступ к бэкапам для любого процесса на сервере. Агент обязан отказать.
func TestAgentRefusesWorldReadableConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	mustWrite(t, cfg, `{"server":"box","repo":{"type":"local","path":"/tmp/x"},
		"plan":{"modules":[{"kind":"files","name":"x","enabled":true,"paths":["/etc"]}]}}`, 0o644)

	out, err := exec.Command(agentPath, "backup", "--config", cfg, "--dry-run").CombinedOutput()
	if err == nil {
		t.Fatal("агент принял конфигурацию с правами 644")
	}
	if !strings.Contains(string(out), "chmod 600") {
		t.Errorf("сообщение не подсказывает, что делать:\n%s", out)
	}
	t.Logf("отказ: %s", firstLine(string(out)))

	if err := os.Chmod(cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	// С правильными правами он должен дойти дальше - до отсутствующего ключа.
	out, err = exec.Command(agentPath, "backup", "--config", cfg, "--dry-run").CombinedOutput()
	if err != nil && strings.Contains(string(out), "chmod 600") {
		t.Errorf("права исправлены, но агент всё ещё жалуется на них:\n%s", out)
	}
}

// --- Вспомогательное ------------------------------------------------------

func mustWrite(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func lookupUser(t *testing.T, name string) (int, int) {
	t.Helper()
	out := run(t, "id", "-u", name)
	var uid, gid int
	fmt.Sscanf(strings.TrimSpace(out), "%d", &uid)
	fmt.Sscanf(strings.TrimSpace(run(t, "id", "-g", name)), "%d", &gid)
	return uid, gid
}

func getxattr(path, name string) (string, error) {
	buf := make([]byte, 256)
	n, err := unix.Lgetxattr(path, name, buf)
	if err != nil {
		return "", err
	}
	return string(buf[:n]), nil
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// Регрессия на критическую находку аудита: ключ с серверным allowlist
// (--allow) не должен позволить выгрузить файл вне разрешённых каталогов -
// в частности, ключ репозитория или /etc/shadow.
func TestAllowlistKeyRefusesArbitraryRead(t *testing.T) {
	if _, err := os.Stat("/root/.ssh/autobak_test"); err != nil {
		t.Skip("ключ не подготовлен")
	}
	alKey := "/root/.ssh/autobak_allow"
	if _, err := os.Stat(alKey); err != nil {
		run(t, "ssh-keygen", "-t", "ed25519", "-N", "", "-C", "allow", "-f", alKey)
		pub, err := os.ReadFile(alKey + ".pub")
		if err != nil {
			t.Fatal(err)
		}
		// Ключ ограничен каталогом /var/www.
		line := sshx.AuthorizedKeyLine(string(pub), agentPath, true, []string{"--allow=/var/www"})
		f, err := os.OpenFile("/root/.ssh/authorized_keys", os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(line + "\n"); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}

	sshArgs := []string{
		"-i", alKey, "-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
		"root@127.0.0.1",
	}

	// Формируем запрос export с планом на /etc/shadow - как это сделал бы
	// похититель ключа. Передаём план файлом на сервере через --plan.
	evilPlan := `{"version":1,"server":"x","modules":[{"kind":"files","name":"evil","enabled":true,"paths":["/etc/shadow"]}]}`
	if err := os.WriteFile("/tmp/evil-plan.json", []byte(evilPlan), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("ssh", append(sshArgs, "autobak-agent export --plan /tmp/evil-plan.json")...).CombinedOutput()
	if err == nil {
		t.Fatal("allow-ключ допустил выгрузку /etc/shadow вне разрешённого каталога")
	}
	if !strings.Contains(string(out), "allow") && !strings.Contains(string(out), "разрешённых") {
		t.Fatalf("отказ не про allowlist:\n%s", out)
	}
	t.Logf("отклонено: %s", firstLine(string(out)))
}
