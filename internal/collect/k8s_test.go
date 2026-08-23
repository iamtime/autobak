package collect

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/iamtime/autobak/internal/plan"
)

// Поля, которые кластер переписывает сам, обязаны вычищаться. Без этого
// каждый бэкап отличался бы от предыдущего целиком, а diff в git-зеркале
// состоял бы из одних resourceVersion.
func TestK8sCleanRemovesVolatileFields(t *testing.T) {
	raw := `{
	  "apiVersion": "apps/v1", "kind": "Deployment",
	  "metadata": {
	    "name": "web", "namespace": "shop",
	    "resourceVersion": "918273", "uid": "8a7c-1122", "generation": 14,
	    "creationTimestamp": "2026-01-02T03:04:05Z",
	    "managedFields": [{"manager": "kubectl"}],
	    "annotations": {
	      "kubectl.kubernetes.io/last-applied-configuration": "{...}",
	      "deployment.kubernetes.io/revision": "7",
	      "мой/аннотация": "оставить"
	    }
	  },
	  "spec": {"replicas": 3},
	  "status": {"readyReplicas": 3}
	}`
	var obj map[string]any
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&obj); err != nil {
		t.Fatal(err)
	}
	clean(obj)

	if _, ok := obj["status"]; ok {
		t.Error("status остался в манифесте")
	}
	meta := obj["metadata"].(map[string]any)
	for _, f := range volatileMeta {
		if _, ok := meta[f]; ok {
			t.Errorf("поле %s не вычищено", f)
		}
	}
	ann := meta["annotations"].(map[string]any)
	if _, ok := ann["kubectl.kubernetes.io/last-applied-configuration"]; ok {
		t.Error("last-applied-configuration остался")
	}
	if ann["мой/аннотация"] != "оставить" {
		t.Error("вычищена пользовательская аннотация")
	}
	// Существенное обязано остаться: без spec манифест бесполезен.
	if obj["spec"].(map[string]any)["replicas"].(json.Number).String() != "3" {
		t.Error("spec пострадал при очистке")
	}
	if meta["name"] != "web" || meta["namespace"] != "shop" {
		t.Error("имя или пространство имён потеряны")
	}
}

func TestK8sManifestPath(t *testing.T) {
	cases := []struct{ ns, kind, name, want string }{
		{"shop", "Deployment", "web", "/@k8s/namespaces/shop/deployment/web.yaml"},
		{"", "Namespace", "shop", "/@k8s/cluster/namespace/shop.yaml"},
		{"kube-system", "Secret", "sa-token", "/@k8s/namespaces/kube-system/secret/sa-token.yaml"},
		// Имена вроде system:node лежат в путях, и двоеточие в имени файла
		// сломало бы восстановление на Windows.
		{"", "ClusterRole", "system:node", "/@k8s/cluster/clusterrole/system_node.yaml"},
	}
	for _, c := range cases {
		if got := manifestPath(c.ns, c.kind, c.name); got != c.want {
			t.Errorf("manifestPath(%q,%q,%q) = %q, ожидалось %q", c.ns, c.kind, c.name, got, c.want)
		}
	}
}

func TestK8sSkipKind(t *testing.T) {
	c := &k8sCollector{m: plan.Module{}}
	// Поды и реплика-сеты воссоздаются контроллерами, хранить их незачем.
	for _, k := range []string{"pods", "events", "replicasets.apps", "endpoints"} {
		if !c.skipKind(k) {
			t.Errorf("тип %s должен пропускаться по умолчанию", k)
		}
	}
	for _, k := range []string{"deployments.apps", "secrets", "configmaps",
		"certificates.cert-manager.io", "persistentvolumeclaims"} {
		if c.skipKind(k) {
			t.Errorf("тип %s пропускать нельзя", k)
		}
	}

	// Явное включение перебивает список по умолчанию.
	c2 := &k8sCollector{m: plan.Module{IncludeKinds: []string{"pods"}}}
	if c2.skipKind("pods") {
		t.Error("явно включённый тип всё равно пропущен")
	}
	// Секреты можно выключить, но только осознанно.
	c3 := &k8sCollector{m: plan.Module{SkipSecrets: true}}
	if !c3.skipKind("secrets") {
		t.Error("SkipSecrets не подействовал")
	}
}

// Полный проход коллектора на подставном kubectl: проверяются разбор
// ответов, отбор типов, фильтр пространств имён и раскладка файлов.
func TestK8sCollectorEndToEnd(t *testing.T) {
	responses := map[string]string{
		"version":                          `{"serverVersion":{"gitVersion":"v1.29.4"}}`,
		"api-resources --namespaced=true":  "deployments.apps\nsecrets\nconfigmaps\npods\n",
		"api-resources --namespaced=false": "namespaces\nclusterroles.rbac.authorization.k8s.io\n",
		"get deployments.apps": `{"items":[
			{"kind":"Deployment","metadata":{"name":"web","namespace":"shop",
			 "resourceVersion":"1","managedFields":[{}]},"spec":{"replicas":2},"status":{}},
			{"kind":"Deployment","metadata":{"name":"cron","namespace":"other"},"spec":{"replicas":1}}]}`,
		"get secrets": `{"items":[
			{"kind":"Secret","metadata":{"name":"db","namespace":"shop"},
			 "data":{"password":"c2VrcmV0"}}]}`,
		"get configmaps": `{"items":[
			{"kind":"ConfigMap","metadata":{"name":"app","namespace":"shop"},"data":{"k":"v"}}]}`,
		"get namespaces": `{"items":[{"kind":"Namespace","metadata":{"name":"shop"}}]}`,
		"get clusterroles.rbac.authorization.k8s.io": `{"items":[]}`,
	}

	c := newK8s(plan.Module{
		Kind: plan.KindK8s, Name: "кластер", Enabled: true,
		Namespaces: []string{"shop"},
	})
	// Сборщик опрашивает типы параллельно, поэтому подставной kubectl
	// вызывается из нескольких горутин. Без замка это гонка в самом
	// тесте - а флакующий тест прячет настоящие гонки в рабочем коде.
	var mu sync.Mutex
	var asked []string
	c.runner = func(_ context.Context, args ...string) ([]byte, error) {
		line := strings.Join(args, " ")
		mu.Lock()
		asked = append(asked, line)
		mu.Unlock()
		// Сопоставление по всем словам ключа, а не по префиксу: настоящие
		// аргументы содержат ещё --verbs=list, -o name и прочее, и точный
		// префикс пришлось бы дублировать из кода коллектора.
		for key, val := range responses {
			if matchesAll(line, key) {
				return []byte(val), nil
			}
		}
		return nil, nil
	}

	s := newTestSink()
	meta, err := c.Collect(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}

	// Поды не запрашивались вовсе - их отсекли до обращения к кластеру.
	for _, a := range asked {
		if strings.HasPrefix(a, "get pods") {
			t.Error("запрошены поды, хотя тип пропускается")
		}
	}

	want := []string{
		"/@k8s/namespaces/shop/deployment/web.yaml",
		"/@k8s/namespaces/shop/secret/db.yaml",
		"/@k8s/namespaces/shop/configmap/app.yaml",
		"/@k8s/cluster/namespace/shop.yaml",
	}
	for _, p := range want {
		if _, ok := s.data[p]; !ok {
			t.Errorf("нет объекта %s (собрано: %v)", p, s.paths())
		}
	}
	// Фильтр пространств имён обязан работать: cron живёт в other.
	if _, ok := s.data["/@k8s/namespaces/other/deployment/cron.yaml"]; ok {
		t.Error("объект из чужого пространства имён попал в снимок")
	}

	// Манифест должен быть валидным YAML с сохранённым содержимым.
	dep := s.data["/@k8s/namespaces/shop/deployment/web.yaml"]
	if !strings.Contains(dep, "replicas: 2") {
		t.Errorf("spec потерян при переводе в YAML:\n%s", dep)
	}
	if strings.Contains(dep, "resourceVersion") || strings.Contains(dep, "managedFields") {
		t.Errorf("изменчивые поля попали в манифест:\n%s", dep)
	}
	if strings.Contains(dep, "status:") {
		t.Errorf("status попал в манифест:\n%s", dep)
	}

	// Секрет сохранён - именно ради этого модуль и нужен.
	if !strings.Contains(s.data["/@k8s/namespaces/shop/secret/db.yaml"], "c2VrcmV0") {
		t.Error("содержимое секрета не сохранено")
	}
	if meta["secrets"].(int) != 1 {
		t.Errorf("счётчик секретов: %v", meta["secrets"])
	}
	if meta["objects"].(int) != 4 {
		t.Errorf("объектов: %v, ожидалось 4", meta["objects"])
	}
	t.Logf("собрано объектов: %v, секретов: %v, типов: %v",
		meta["objects"], meta["secrets"], meta["kinds"])
}

func matchesAll(line, key string) bool {
	for _, w := range strings.Fields(key) {
		if !strings.Contains(line, w) {
			return false
		}
	}
	return true
}

// Числа не должны превращаться в экспоненциальную запись: replicas: 3e+00
// сломает kubectl apply и завалит diff мусором.
func TestK8sPreservesIntegers(t *testing.T) {
	items, err := splitList([]byte(`{"items":[{"kind":"X","metadata":{"name":"a"},
		"spec":{"replicas":10,"port":8080,"ratio":0.5}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("объектов: %d", len(items))
	}
	spec := items[0]["spec"].(map[string]any)
	// Целые обязаны остаться целыми, дробные - дробными: и то и другое
	// yaml.v3 напишет без кавычек и без экспоненты.
	if v, ok := spec["replicas"].(int64); !ok || v != 10 {
		t.Errorf("replicas = %#v, ожидалось int64(10)", spec["replicas"])
	}
	if v, ok := spec["port"].(int64); !ok || v != 8080 {
		t.Errorf("port = %#v, ожидалось int64(8080)", spec["port"])
	}
	if v, ok := spec["ratio"].(float64); !ok || v != 0.5 {
		t.Errorf("ratio = %#v, ожидалось float64(0.5)", spec["ratio"])
	}
}
