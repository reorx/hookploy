package ops

import "sort"

// OpInfo describes one op of the vocabulary: its name, a zero-valued Args
// instance to reflect over, and the prose used by generated artifacts
// (JSON Schema descriptions, docs).
type OpInfo struct {
	Name      string
	Args      Args              // fresh zero-value Args of this op
	Defaults  Args              // same type with setDefaults() applied (== Args when the op has none)
	Doc       string            // one-line description of the op
	FieldDocs map[string]string // exported field name → description
}

// opDoc is the hand-written prose for one op. Keep it in sync with the doc
// comments in types.go and the op vocabulary table in PRD §4 — catalog_test
// fails when a registered op or an exported arg field is left undocumented.
type opDoc struct {
	doc    string
	fields map[string]string
}

var opDocs = map[string]opDoc{
	"image.pin": {
		doc: "digest 锁定部署：pull payload.digest 指定的镜像并把本地 :latest 指向它，流水线最后一个 compose.up 之后自动验证（零参数）",
	},
	"image.extract": {
		doc: "从镜像抽取文件到服务目录，近原子交换",
		fields: map[string]string{
			"From":  "镜像内的源路径（必填）",
			"To":    "服务目录下的目标路径（必填）",
			"Image": "抽取的镜像，默认为服务 image: 锁定后的本地 :latest",
			"Pull":  "抽取前先 docker pull 该镜像",
		},
	},
	"artifact.extract": {
		doc: "下载 artifact，校验 sha256 后解压（tar.gz / zip）并近原子交换到目录",
		fields: map[string]string{
			"URL":    "artifact 下载地址，支持 ${payload.x} 插值（必填）",
			"SHA256": "artifact 的 sha256 校验和（必填：artifact 来自公网 URL，无校验等于接受任意代码）",
			"To":     "服务目录下的解压目标路径（必填）",
		},
	},
	"compose.pull": {
		doc: "docker compose pull",
		fields: map[string]string{
			"Services": "只拉取指定 compose service，省略为全部",
		},
	},
	"compose.up": {
		doc: "docker compose up -d",
		fields: map[string]string{
			"ForceRecreate": "传入 --force-recreate，强制重建容器",
			"Services":      "只启动指定 compose service，省略为全部",
		},
	},
	"compose.run": {
		doc: "docker compose run --rm：起新容器跑一次性命令",
		fields: map[string]string{
			"Service": "compose service 名（必填）",
			"Argv":    "命令 argv 数组，直接 exec 不经 shell（必填）",
		},
	},
	"compose.exec": {
		doc: "docker compose exec -T：进运行中的容器执行命令",
		fields: map[string]string{
			"Service": "compose service 名（必填）",
			"Argv":    "命令 argv 数组，直接 exec 不经 shell（必填）",
		},
	},
	"compose.restart": {
		doc: "docker compose restart",
		fields: map[string]string{
			"Services": "只重启指定 compose service，省略为全部",
		},
	},
	"env.require": {
		doc: "断言 env 文件中指定 key 已填非空值，否则失败",
		fields: map[string]string{
			"File": "env 文件路径，相对服务目录（必填）",
			"Keys": "必须存在且非空的 key 列表（必填）",
		},
	},
	"env.write": {
		doc: "向指定文件写入/更新 KEY=VALUE 行",
		fields: map[string]string{
			"File": "env 文件路径，相对服务目录（必填）",
			"Set":  "要写入的 KEY=VALUE 映射，值支持 ${payload.x} 插值（必填）",
		},
	},
	"healthcheck": {
		doc: "按 interval 轮询 HTTP 端点直至返回 expect 状态码，retries 耗尽即失败",
		fields: map[string]string{
			"URL":      "被轮询的 HTTP 地址（必填）",
			"Expect":   "期望的 HTTP 状态码",
			"Retries":  "最大重试次数",
			"Interval": "两次轮询之间的间隔，Go duration 字符串（如 3s、1m30s）",
		},
	},
	"run": {
		doc: "在服务目录下执行一条命令（argv 数组，不经 shell）；op 词汇表未覆盖场景的逃生舱",
		fields: map[string]string{
			"Argv": "命令 argv 数组，直接 exec 不经 shell（必填）",
		},
	},
}

// Catalog returns every registered op with its documentation, sorted by
// name so generated artifacts are byte-stable.
func Catalog() []OpInfo {
	infos := make([]OpInfo, 0, len(registry))
	for name, construct := range registry {
		d := opDocs[name]
		defaults := construct()
		if v, ok := defaults.(defaulter); ok {
			v.setDefaults()
		}
		infos = append(infos, OpInfo{
			Name:      name,
			Args:      construct(),
			Defaults:  defaults,
			Doc:       d.doc,
			FieldDocs: d.fields,
		})
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
	return infos
}
