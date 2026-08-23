package collect

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/iamtime/autobak/internal/plan"
	"gopkg.in/yaml.v3"
)

const VirtualK8s = "/@k8s"

// k8sCollector сохраняет состояние кластера как набор манифестов.
//
// Что сохраняется и почему именно так:
//
//   - Ресурсы - по одному файлу на объект. Не одним архивом: пофайловая
//     раскладка даёт и дедупликацию (изменился один Deployment - приехал
//     один файл), и читаемый diff в git-зеркале.
//   - Секреты - по умолчанию да. Это единственное, чего нет в вашем
//     GitOps-репозитории, потому что туда их сознательно не кладут.
//     Потерять их значит перевыпускать все пароли и сертификаты разом.
//   - Данные томов - нет. Для этого нужны снимки CSI и привилегированные
//     поды; этим занимается Velero, и повторять его здесь незачем.
//     Если том лежит на узле как hostPath или примонтирован по NFS,
//     его забирает обычный файловый модуль.
type k8sCollector struct {
	m       plan.Module
	kubectl string
	base    []string

	// runner подменяется в тестах. Иначе проверить разбор ответов,
	// очистку полей и раскладку файлов можно было бы только имея под
	// рукой живой кластер, то есть практически никогда.
	runner func(ctx context.Context, args ...string) ([]byte, error)
}

func newK8s(m plan.Module) *k8sCollector { return &k8sCollector{m: m} }

func (c *k8sCollector) Kind() plan.Kind { return plan.KindK8s }
func (c *k8sCollector) Name() string    { return c.m.Name }

// ephemeralKinds - то, что не имеет смысла хранить.
//
// Под, восстановленный из манифеста, не воспроизводит ничего: его заново
// создаст контроллер, а сам манифест меняется при каждом перезапуске и
// только засоряет историю. То же с ReplicaSet (их создаёт Deployment),
// событиями, эндпоинтами и лизами.
var ephemeralKinds = []string{
	"pods", "events", "events.events.k8s.io",
	"replicasets.apps", "controllerrevisions.apps",
	"endpoints", "endpointslices.discovery.k8s.io",
	"leases.coordination.k8s.io",
	"componentstatuses", "bindings",
	"tokenreviews.authentication.k8s.io",
	"localsubjectaccessreviews.authorization.k8s.io",
	"selfsubjectaccessreviews.authorization.k8s.io",
	"selfsubjectrulesreviews.authorization.k8s.io",
	"subjectaccessreviews.authorization.k8s.io",
	"certificatesigningrequests.certificates.k8s.io",
	"nodes.metrics.k8s.io", "pods.metrics.k8s.io",
}

func (c *k8sCollector) Collect(ctx context.Context, s Sink) (map[string]any, error) {
	if c.runner == nil {
		var err error
		if c.kubectl, err = lookPath("kubectl",
			"установите kubectl или выключите модуль Kubernetes"); err != nil {
			return nil, err
		}
	}
	if c.m.Kubeconfig != "" {
		c.base = append(c.base, "--kubeconfig="+c.m.Kubeconfig)
	}
	if c.m.Context != "" {
		c.base = append(c.base, "--context="+c.m.Context)
	}

	if out, err := c.run(ctx, "version", "--output=json"); err != nil {
		return nil, fmt.Errorf("autobak: кластер недоступен: %w", err)
	} else if v := serverVersion(out); v != "" {
		s.Logf("info", "кластер Kubernetes %s", v)
	}

	kinds, err := c.kindsToCollect(ctx, s)
	if err != nil {
		return nil, err
	}

	objects, secrets, failed := c.fetchAll(ctx, s, kinds)

	// Порядок фиксирован, иначе дерево снимка менялось бы от запуска
	// к запуску и перестало бы дедуплицироваться.
	paths := make([]string, 0, len(objects))
	for p := range objects {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		n := virtualNode(p, c.m.Name)
		if err := s.File(n, bytes.NewReader(objects[p])); err != nil {
			return nil, err
		}
	}

	meta := map[string]any{
		"objects": len(objects),
		"secrets": secrets,
		"kinds":   len(kinds),
	}
	if c.m.EtcdSnapshot {
		if err := c.dumpEtcd(ctx, s); err != nil {
			s.Logf("error", "снимок etcd не сделан: %v", err)
			meta["etcd_error"] = err.Error()
		} else {
			meta["etcd"] = true
		}
	}
	if len(failed) > 0 {
		meta["failed_kinds"] = failed
		s.Logf("warn", "не удалось выгрузить типы: %s", strings.Join(failed, ", "))
	}
	return meta, nil
}

func (c *k8sCollector) run(ctx context.Context, args ...string) ([]byte, error) {
	if c.runner != nil {
		return c.runner(ctx, args...)
	}
	return runCapture(ctx, c.kubectl, append(append([]string{}, c.base...), args...)...)
}

func serverVersion(out []byte) string {
	var v struct {
		ServerVersion struct{ GitVersion string } `json:"serverVersion"`
	}
	if json.Unmarshal(out, &v) == nil {
		return v.ServerVersion.GitVersion
	}
	return ""
}

// kindsToCollect спрашивает у самого кластера, какие типы в нём есть.
//
// Не жёсткий список: половина ценного в кластере - это CRD (сертификаты
// cert-manager, ingress-роуты, кастомные операторы), и перечислить их
// заранее невозможно.
func (c *k8sCollector) kindsToCollect(ctx context.Context, s Sink) ([]k8sKind, error) {
	var out []k8sKind
	for _, scope := range []struct {
		flag       string
		namespaced bool
	}{{"--namespaced=true", true}, {"--namespaced=false", false}} {
		raw, err := c.run(ctx, "api-resources", "--verbs=list", scope.flag, "-o", "name")
		if err != nil {
			return nil, fmt.Errorf("autobak: не получить список типов: %w", err)
		}
		for _, name := range trimLines(raw) {
			if c.skipKind(name) {
				continue
			}
			out = append(out, k8sKind{Name: name, Namespaced: scope.namespaced})
		}
	}
	s.Logf("info", "типов ресурсов к выгрузке: %d", len(out))
	return out, nil
}

type k8sKind struct {
	Name       string
	Namespaced bool
}

func (c *k8sCollector) skipKind(name string) bool {
	if slices.Contains(c.m.ExcludeKinds, name) {
		return true
	}
	if slices.Contains(c.m.IncludeKinds, name) {
		return false
	}
	if c.m.SkipSecrets && name == "secrets" {
		return true
	}
	return slices.Contains(ephemeralKinds, name)
}

// fetchAll забирает объекты всех типов.
//
// Запросы идут параллельно: типов в кластере с операторами бывает под
// сотню, и последовательный опрос превратился бы в минуту ожидания.
// Запись в Sink при этом остаётся последовательной - он не рассчитан
// на несколько горутин.
func (c *k8sCollector) fetchAll(ctx context.Context, s Sink, kinds []k8sKind) (map[string][]byte, int, []string) {
	type result struct {
		kind k8sKind
		raw  []byte
		err  error
	}
	results := make([]result, len(kinds))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup

	for i, k := range kinds {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			args := []string{"get", k.Name, "-o", "json", "--ignore-not-found"}
			if k.Namespaced {
				if len(c.m.Namespaces) == 1 {
					args = append(args, "-n", c.m.Namespaces[0])
				} else {
					args = append(args, "--all-namespaces")
				}
			}
			raw, err := c.run(ctx, args...)
			results[i] = result{kind: k, raw: raw, err: err}
		}()
	}
	wg.Wait()

	objects := map[string][]byte{}
	var failed []string
	secrets := 0

	for _, res := range results {
		if res.err != nil {
			// Часть API (метрики, агрегированные серверы) отвечает ошибкой,
			// даже когда числится в списке типов. Это не повод срывать бэкап.
			failed = append(failed, res.kind.Name)
			continue
		}
		items, err := splitList(res.raw)
		if err != nil {
			failed = append(failed, res.kind.Name)
			continue
		}
		for _, item := range items {
			ns, name, kind := identify(item)
			if name == "" {
				continue
			}
			if !c.wantNamespace(ns) {
				continue
			}
			clean(item)
			doc, err := yaml.Marshal(item)
			if err != nil {
				continue
			}
			p := manifestPath(ns, kind, name)
			objects[p] = doc
			if strings.EqualFold(kind, "Secret") {
				secrets++
			}
		}
	}
	if secrets > 0 {
		s.Logf("info", "секретов сохранено: %d", secrets)
	}
	return objects, secrets, failed
}

func (c *k8sCollector) wantNamespace(ns string) bool {
	if ns == "" || len(c.m.Namespaces) == 0 {
		return true
	}
	return slices.Contains(c.m.Namespaces, ns)
}

func manifestPath(ns, kind, name string) string {
	kind = safeFileName(strings.ToLower(kind))
	safe := safeFileName(name)
	if ns == "" {
		return path.Join(VirtualK8s, "cluster", kind, safe+".yaml")
	}
	// Имя namespace тоже санитизируем: хотя API-сервер и ограничивает его,
	// полагаться на это при построении пути в снимке не стоит - косая черта
	// или переход вверх в имени промахнулись бы мимо каталога.
	return path.Join(VirtualK8s, "namespaces", safeFileName(ns), kind, safe+".yaml")
}

// splitList разбирает ответ kubectl на отдельные объекты.
func splitList(raw []byte) ([]map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	// UseNumber сохраняет целые как целые: без него replicas: 3
	// превратилось бы в 3e+00 и давало бы разный diff на пустом месте.
	dec.UseNumber()
	var root map[string]any
	if err := dec.Decode(&root); err != nil {
		return nil, err
	}
	rawItems, ok := root["items"].([]any)
	if !ok {
		// Ответ на одиночный объект, а не список.
		return []map[string]any{numbersToNative(root).(map[string]any)}, nil
	}
	out := make([]map[string]any, 0, len(rawItems))
	for _, it := range rawItems {
		if m, ok := it.(map[string]any); ok {
			out = append(out, numbersToNative(m).(map[string]any))
		}
	}
	return out, nil
}

// numbersToNative переводит json.Number в настоящие числа.
//
// json.Number - это строковый тип, и yaml.v3 пишет его в кавычках:
// replicas: "2". Такой манифест kubectl отвергнет, потому что схема
// ждёт там целое. Разбирать же JSON без UseNumber тоже нельзя: тогда
// 8080 превратится в 8.08e+03 и сломается по-другому.
func numbersToNative(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			x[k] = numbersToNative(e)
		}
		return x
	case []any:
		for i, e := range x {
			x[i] = numbersToNative(e)
		}
		return x
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i
		}
		if f, err := x.Float64(); err == nil {
			return f
		}
		return x.String()
	}
	return v
}

func identify(obj map[string]any) (ns, name, kind string) {
	kind, _ = obj["kind"].(string)
	meta, _ := obj["metadata"].(map[string]any)
	if meta == nil {
		return "", "", kind
	}
	name, _ = meta["name"].(string)
	ns, _ = meta["namespace"].(string)
	return ns, name, kind
}

// volatileMeta - поля, которые кластер переписывает сам.
//
// Без их удаления каждый бэкап отличался бы от предыдущего целиком:
// resourceVersion меняется при любой записи, managedFields распухает от
// каждого apply, а status живёт своей жизнью. Ровно та же причина, по
// которой дамп PostgreSQL берётся без встроенного сжатия.
var volatileMeta = []string{
	"resourceVersion", "uid", "generation", "creationTimestamp",
	"managedFields", "selfLink",
}

func clean(obj map[string]any) {
	delete(obj, "status")
	meta, _ := obj["metadata"].(map[string]any)
	if meta == nil {
		return
	}
	for _, f := range volatileMeta {
		delete(meta, f)
	}
	if ann, ok := meta["annotations"].(map[string]any); ok {
		delete(ann, "kubectl.kubernetes.io/last-applied-configuration")
		delete(ann, "deployment.kubernetes.io/revision")
		if len(ann) == 0 {
			delete(meta, "annotations")
		}
	}
}

// dumpEtcd снимает слепок всего хранилища кластера.
//
// etcdctl умеет писать только в файл, поэтому здесь единственное место,
// где на диск сервера что-то кладётся временно. Слепок весит сотни
// мегабайт, и модуль выключен по умолчанию: манифестов хватает для
// восстановления рабочих нагрузок, а etcd нужен для возрождения самого
// управляющего узла.
func (c *k8sCollector) dumpEtcd(ctx context.Context, s Sink) error {
	bin, err := lookPath("etcdctl", "снимок etcd требует etcdctl на управляющем узле")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp("", "autobak-etcd-*.db")
	if err != nil {
		return err
	}
	tmp := f.Name()
	f.Close()
	defer os.Remove(tmp)

	endpoint := c.m.EtcdEndpoint
	if endpoint == "" {
		endpoint = "https://127.0.0.1:2379"
	}
	args := []string{"snapshot", "save", tmp, "--endpoints=" + endpoint}
	// Пути сертификатов kubeadm - самый частый случай. Если их нет,
	// etcdctl возьмёт настройки из окружения.
	for flag, file := range map[string]string{
		"--cacert": "/etc/kubernetes/pki/etcd/ca.crt",
		"--cert":   "/etc/kubernetes/pki/etcd/server.crt",
		"--key":    "/etc/kubernetes/pki/etcd/server.key",
	} {
		if _, err := os.Stat(file); err == nil {
			args = append(args, flag+"="+file)
		}
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(), "ETCDCTL_API=3")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, stderrTail(out, 500))
	}

	src, err := os.Open(tmp)
	if err != nil {
		return err
	}
	defer src.Close()
	st, _ := src.Stat()
	s.Logf("info", "снимок etcd: %d МБ", st.Size()/(1<<20))

	n := virtualNode(path.Join(VirtualK8s, "etcd", "snapshot.db"), c.m.Name)
	return s.File(n, src)
}
