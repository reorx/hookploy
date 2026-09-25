package config

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/reorx/hookploy/internal/model"
	"github.com/reorx/hookploy/internal/ops"
)

// jsonSchema is the minimal draft-07 node this generator emits. Field order
// here is the emitted key order; map-valued keywords are sorted by
// encoding/json, so the output is byte-stable.
type jsonSchema struct {
	Schema      string `json:"$schema,omitempty"`
	Ref         string `json:"$ref,omitempty"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Type        string `json:"type,omitempty"`

	Properties           map[string]*jsonSchema `json:"properties,omitempty"`
	Required             []string               `json:"required,omitempty"`
	AdditionalProperties any                    `json:"additionalProperties,omitempty"`
	MinProperties        *int                   `json:"minProperties,omitempty"`
	MaxProperties        *int                   `json:"maxProperties,omitempty"`
	Dependencies         map[string][]string    `json:"dependencies,omitempty"`

	Items    *jsonSchema `json:"items,omitempty"`
	MinItems *int        `json:"minItems,omitempty"`
	Minimum  *int        `json:"minimum,omitempty"`

	AllOf []*jsonSchema `json:"allOf,omitempty"`
	OneOf []*jsonSchema `json:"oneOf,omitempty"`
	Not   *jsonSchema   `json:"not,omitempty"`

	Enum    []any  `json:"enum,omitempty"`
	Pattern string `json:"pattern,omitempty"`
	Default any    `json:"default,omitempty"`

	Definitions map[string]*jsonSchema `json:"definitions,omitempty"`
}

func intp(v int) *int { return &v }

// durationPattern matches what time.ParseDuration accepts (the form
// model.Duration decodes from YAML): an optionally signed sequence of
// decimal numbers with unit suffixes, plus the bare "0". Each number may drop
// its integer part (".5s") or its fractional part ("1.s") but not both, and
// micro accepts both the micro sign U+00B5 (µ) and Greek small mu U+03BC (μ).
// Keep this in sync with ParseDuration — a stricter pattern would flag
// configs that load fine.
const durationPattern = `^[+-]?(0|(([0-9]+(\.[0-9]*)?|\.[0-9]+)(ns|us|µs|μs|ms|s|m|h))+)$`

// durationRef references the duration definition while keeping annotations
// usable: draft-07 implementations ignore keywords sitting next to $ref, so
// the reference has to move under allOf.
func durationRef(description string) *jsonSchema {
	return &jsonSchema{
		AllOf:       []*jsonSchema{{Ref: "#/definitions/duration"}},
		Description: description,
	}
}

const schemaTitle = "hookploy.yaml"

const schemaDescription = `hookploy 的服务定义 SSOT。` +
	`本 schema 是宽松上界：它覆盖磁盘形态（字段名、类型、op 参数、互斥结构），` +
	`但不做跨字段的语义校验（服务器引用是否存在、rollout 是否恰好覆盖全部实例、` +
	`image.pin 是否有后续 compose.up 等）。最终以 ` + "`hookploy validate`" + ` 为准。`

// JSONSchema renders the draft-07 JSON Schema of hookploy.yaml. The op part
// is derived from ops.Catalog() by reflection, so a new op shows up in the
// schema automatically.
func JSONSchema() ([]byte, error) {
	root := &jsonSchema{
		Schema:      "http://json-schema.org/draft-07/schema#",
		Title:       schemaTitle,
		Description: schemaDescription,
		Type:        "object",
		Properties: map[string]*jsonSchema{
			"listen": {
				Type:        "object",
				Description: "main 的监听地址。",
				Properties: map[string]*jsonSchema{
					"http": {Type: "string", Description: "webhook 与状态 API 的监听地址，默认 127.0.0.1:9100。"},
					"grpc": {Type: "string", Description: "edge 接入的 gRPC 监听地址，默认 127.0.0.1:9101。"},
				},
				AdditionalProperties: false,
			},
			"db": {
				Type:        "string",
				Description: "SQLite 数据库路径，默认与本文件同目录的 hookploy.db。",
			},
			"webui": {
				Type:        "boolean",
				Description: "是否挂载内置只读 Web UI（/ui/），默认 true；false 时 /ui/ 与根路径跳转均不注册，改动需重启 main 生效。",
			},
			"github": {
				Type:        "object",
				Description: "GitHub 集成：接收 workflow_run webhook，在 Web UI 展示构建状态。",
				Properties: map[string]*jsonSchema{
					"webhook_secret": {
						Type:        "string",
						Description: "GitHub webhook 的 HMAC secret（X-Hub-Signature-256 校验）；未配置时 POST /github/webhook 端点关闭（404）。",
					},
				},
				AdditionalProperties: false,
			},
			"notify": {
				Type:        "object",
				Description: "部署结果通知。同一时刻只有一个 provider 生效；省略 provider 等于关闭通知。凭据明文写在这里，用时现读，不出现在任何 API / Web UI / --json 输出中。",
				Properties: map[string]*jsonSchema{
					"provider": {
						Type: "string",
						// center 先列进来：schema 是宽松上界，"尚未实现" 由
						// hookploy validate 报，第二阶段接入时这里无需改动。
						Enum:        []any{"telegram", "center"},
						Description: "通知后端；省略 = 不发通知。center 为统一通知中心，尚未实现。",
					},
					"base_url": {
						Type:        "string",
						Description: "Web UI 对外可访问的根地址，用于在消息里拼出 /ui/deploys/<id> 链接；留空则消息不带链接。",
					},
					"events": {
						Type:  "array",
						Items: &jsonSchema{Type: "string", Enum: eventKindEnum()},
						Description: `要推送的事件，省略时默认为 ["deploy.failed", "main.started", "edge.offline", "edge.online"]。` +
							`deploy.* 可被服务级 notify.events 覆盖；main.* / edge.* 是节点事件，不属于任何服务，只认这一份全局列表。`,
					},
					"edge_offline_after": durationRef(
						"edge 断连超过该时长即推送 edge.offline，恢复连接时推送 edge.online；默认 5m。" +
							"应大于 edge 重连宽限（60s），否则一次正常的流断也会告警。"),
					"telegram": {
						Type:        "object",
						Description: "provider: telegram 时必填（缺任一项则 hookploy validate 失败）。",
						Properties: map[string]*jsonSchema{
							"bot_token": {Type: "string", Description: "Telegram Bot API token。"},
							"chat_id":   {Type: "string", Description: "目标频道/群组的 chat id。"},
						},
						AdditionalProperties: false,
					},
				},
				AdditionalProperties: false,
			},
			"servers": {
				Type:                 "object",
				Description:          "部署目标服务器。键为 server 名，edge 的身份由 server token 的 subject 推导。",
				AdditionalProperties: &jsonSchema{Ref: "#/definitions/server"},
			},
			"defaults": {
				Type:        "object",
				Description: "全局默认值。",
				Properties: map[string]*jsonSchema{
					"timeout": durationRef("单次执行的默认超时，默认 10m。"),
				},
				AdditionalProperties: false,
			},
			"services": {
				Type:                 "object",
				Description:          "服务定义。键为服务名，也是 webhook 路径 /hooks/<service>。",
				AdditionalProperties: &jsonSchema{Ref: "#/definitions/service"},
			},
		},
		AdditionalProperties: false,
		Definitions: map[string]*jsonSchema{
			"duration": {
				Type:        "string",
				Description: "Go duration 字符串，如 30s、10m、1h30m。",
				Pattern:     durationPattern,
			},
			"server": {
				Type:        "object",
				Description: "一台部署目标服务器。",
				Properties: map[string]*jsonSchema{
					"local": {Type: "boolean", Description: "true 表示由 main 内建 executor 就地执行，不需要 edge 接入。"},
				},
				AdditionalProperties: false,
			},
			"instance": {
				Type:        "object",
				Description: "服务的一个部署实例。",
				Properties: map[string]*jsonSchema{
					"server": {Type: "string", Description: "所在服务器名，必须在顶层 servers 中声明。"},
					"dir":    {Type: "string", Description: "服务目录，省略时继承服务级 dir。"},
				},
				Required:             []string{"server"},
				AdditionalProperties: false,
			},
			"service": serviceSchema(),
			"step":    stepSchema(),
		},
	}
	return json.MarshalIndent(root, "", "  ")
}

// eventKindEnum renders the notify vocabulary as a schema enum, so a new
// model.EventKind shows up in the schema without touching this file.
func eventKindEnum() []any { return kindEnum("") }

// deployEventKindEnum is the service-level subset: a service's notify.events
// may only name deploy outcomes, which the loader also enforces.
func deployEventKindEnum() []any { return kindEnum(model.ScopeDeploy) }

// kindEnum lists the vocabulary, narrowed to one scope when only is set.
func kindEnum(only model.EventScope) []any {
	out := []any{}
	for _, k := range model.EventKinds() {
		if only != "" && k.Scope() != only {
			continue
		}
		out = append(out, string(k))
	}
	return out
}

func serviceSchema() *jsonSchema {
	pipeline := func(desc string) *jsonSchema {
		return &jsonSchema{
			Type:        "array",
			Description: desc,
			Items:       &jsonSchema{Ref: "#/definitions/step"},
			MinItems:    intp(1),
		}
	}
	return &jsonSchema{
		Type: "object",
		Description: "一个服务的定义。单机形态用 server + dir，多机形态用 instances（两者互斥）；" +
			"rollout 只在 instances 形态下有意义。",
		Properties: map[string]*jsonSchema{
			"server": {Type: "string", Description: "单机语法糖：部署到该服务器，等价于「单实例 + 单波」，与 instances 互斥。"},
			"dir":    {Type: "string", Description: "服务目录（compose 文件所在处）；instances 形态下作为各实例 dir 的默认值。"},
			"image":  {Type: "string", Description: "服务镜像仓库地址，image.pin / image.extract 的作用对象。"},
			"webhook": {
				Type:        "boolean",
				Description: "是否接受 webhook 触发，默认 true；false 表示只能手动部署。",
			},
			"github_repo": {
				Type:        "string",
				Pattern:     `^[^/\s]+/[^/\s]+$`,
				Description: "关联的 GitHub 仓库（owner/repo），用于把 workflow_run 事件关联到本服务并在 UI 展示构建。",
			},
			"notify": {
				Type:        "object",
				Description: "覆盖全局通知策略，仅本服务生效。",
				Properties: map[string]*jsonSchema{
					"enabled": {Type: "boolean", Description: "false 表示本服务完全静音，默认 true。"},
					"events": {
						Type:        "array",
						Items:       &jsonSchema{Type: "string", Enum: deployEventKindEnum()},
						Description: "替换（而非追加）本服务的事件列表；省略表示继承全局 notify.events 里的 deploy.* 部分。仅接受 deploy.* 事件。",
					},
				},
				AdditionalProperties: false,
			},
			"timeout": durationRef("单次执行超时，覆盖 defaults.timeout。"),
			"deploy":  pipeline("默认部署流水线，webhook 触发时执行。"),
			"tasks": {
				Type:                 "object",
				Description:          "具名任务：属于该服务但不随 webhook 触发，仅 `hookploy task <service> <name>` 手动执行。",
				AdditionalProperties: pipeline("任务流水线。"),
			},
			"instances": {
				Type:                 "object",
				Description:          "多机形态：键为实例名，同一条 deploy 流水线在每个实例的机器上执行。与 server 互斥。",
				AdditionalProperties: &jsonSchema{Ref: "#/definitions/instance"},
				MinProperties:        intp(1),
			},
			"rollout": {
				Type: "array",
				Description: "发布波次顺序：标量 = 单实例波，列表 = 并行波；波 k 全部成功后波 k+1 才启动。" +
					"省略时按 instances 声明顺序逐实例串行。必须恰好覆盖每个实例一次（该项由 hookploy validate 校验）。",
				Items: &jsonSchema{
					OneOf: []*jsonSchema{
						{Type: "string", Description: "单实例波。"},
						{Type: "array", Description: "并行波。", Items: &jsonSchema{Type: "string"}, MinItems: intp(1)},
					},
				},
			},
		},
		Required:             []string{"deploy"},
		AdditionalProperties: false,
		Dependencies:         map[string][]string{"rollout": {"instances"}},
		OneOf: []*jsonSchema{
			{
				Title:    "单机（server + dir）",
				Required: []string{"server", "dir"},
				Not:      &jsonSchema{Required: []string{"instances"}},
			},
			{
				Title:    "多机（instances）",
				Required: []string{"instances"},
				Not:      &jsonSchema{Required: []string{"server"}},
			},
		},
	}
}

// stepSchema builds the three step forms from the op catalog: a bare string
// (ops whose zero-value Args validate), a single-key map (op name → args),
// and that map plus modifiers (on / timeout / retries). The modifier form is
// a branch of its own whose nested oneOf holds one `required: [op]` per op, so
// "exactly one op name" stays expressible however many modifiers are present
// — and the ops that may not be retried carry a `not: required retries`.
func stepSchema() *jsonSchema {
	var zeroArgOps []any
	var oneOp []*jsonSchema
	argMaps := map[string]*jsonSchema{}
	for _, info := range ops.Catalog() {
		args := opArgsSchema(info)
		if info.Defaults.Validate() == nil {
			zeroArgOps = append(zeroArgOps, info.Name)
			// `- image.pin:` with an empty body is how a no-arg op carries a
			// modifier, so its args node has to accept null as well.
			args = &jsonSchema{
				Description: info.Doc,
				OneOf:       []*jsonSchema{args, {Type: "null", Description: "无参形式（配修饰符时用）。"}},
			}
		}
		argMaps[info.Name] = args
		branch := &jsonSchema{Required: []string{info.Name}}
		if !ops.Retryable(info.Name) {
			branch.Not = &jsonSchema{Required: []string{ops.RetriesKey}}
		}
		oneOp = append(oneOp, branch)
	}

	modified := map[string]*jsonSchema{
		ops.OnKey:      onSchema(),
		ops.TimeoutKey: timeoutSchema(),
		ops.RetriesKey: retriesSchema(),
	}
	for name, args := range argMaps {
		modified[name] = args
	}
	return &jsonSchema{
		Description: "流水线的一步：字符串 = 无参 op，单键 map = op 名 → 参数，再加 on / timeout / retries 修饰符。",
		OneOf: []*jsonSchema{
			{
				Type:        "string",
				Description: "无参形式，仅限所有参数都可省略的 op。",
				Enum:        zeroArgOps,
			},
			{
				Type:                 "object",
				Description:          "带参形式：恰好一个键，即 op 名。",
				Properties:           argMaps,
				AdditionalProperties: false,
				MinProperties:        intp(1),
				MaxProperties:        intp(1),
			},
			{
				Type:                 "object",
				Description:          "带修饰符的形式：一个 op 名加上 on / timeout / retries 中的一个或多个。",
				Properties:           modified,
				AdditionalProperties: false,
				MinProperties:        intp(2),
				OneOf:                oneOp,
			},
		},
	}
}

// timeoutSchema describes the `timeout:` modifier.
func timeoutSchema() *jsonSchema {
	return durationRef("这一步每次尝试的超时，到点即取消（杀掉子进程）；省略则只受服务 timeout 约束。" +
		"timeout × 尝试次数 + 尝试间隔（5s）不得超过服务 timeout。")
}

// retriesSchema describes the `retries:` modifier.
func retriesSchema() *jsonSchema {
	return &jsonSchema{
		Type: "integer",
		Description: "失败（含 timeout）后重跑的次数，每次都是全新执行，间隔 5s。" +
			"仅 image.pin / compose.pull / artifact.extract 可用；image.pin 与 artifact.extract 省略时默认 2，compose.pull 默认 0。",
		Minimum: intp(0),
	}
}

// onSchema describes the `on:` modifier: instance names, one or many.
func onSchema() *jsonSchema {
	return &jsonSchema{
		Description: "限定这一步只在列出的 instance 上执行（instance 名，不是 server 名）；" +
			"省略则每个 instance 都执行。仅 deploy 流水线可用——tasks 用 --instance 选目标。",
		OneOf: []*jsonSchema{
			{Type: "string", Description: "单个 instance 名。"},
			{
				Type:        "array",
				Description: "多个 instance 名。",
				Items:       &jsonSchema{Type: "string"},
				MinItems:    intp(1),
			},
		},
	}
}

// opArgsSchema derives one op's args schema from its Args struct: yaml tags
// give the key names, the json tag's omitempty encodes whether the field is
// required, and the catalog's Defaults instance supplies default values.
func opArgsSchema(info ops.OpInfo) *jsonSchema {
	zero := reflect.ValueOf(info.Args).Elem()
	defaults := reflect.ValueOf(info.Defaults).Elem()
	st := zero.Type()

	schema := &jsonSchema{
		Type:                 "object",
		Description:          info.Doc,
		Properties:           map[string]*jsonSchema{},
		AdditionalProperties: false,
	}
	for i := 0; i < st.NumField(); i++ {
		f := st.Field(i)
		if f.PkgPath != "" { // unexported
			continue
		}
		name := tagName(f.Tag.Get("yaml"), f.Name)
		if name == "-" {
			continue
		}
		fs := goTypeSchema(info.Name, f)
		fs.Description = info.FieldDocs[f.Name]
		if def := defaults.Field(i); !def.IsZero() && !reflect.DeepEqual(def.Interface(), zero.Field(i).Interface()) {
			fs.Default = defaultValue(def)
		}
		schema.Properties[name] = fs
		if !strings.Contains(f.Tag.Get("json"), ",omitempty") {
			schema.Required = append(schema.Required, name)
		}
	}
	sort.Strings(schema.Required)
	return schema
}

// goTypeSchema maps a Go arg field type onto a JSON Schema node. Unmapped
// types panic on purpose: a new arg type must be handled explicitly rather
// than silently degrade the published schema.
func goTypeSchema(op string, f reflect.StructField) *jsonSchema {
	if f.Type == reflect.TypeOf(model.Duration(0)) {
		// allOf-wrapped so the caller's description/default land on a node
		// that is not a bare $ref (draft-07 ignores $ref's siblings).
		return durationRef("")
	}
	switch f.Type.Kind() {
	case reflect.String:
		return &jsonSchema{Type: "string"}
	case reflect.Bool:
		return &jsonSchema{Type: "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return &jsonSchema{Type: "integer"}
	case reflect.Slice:
		if f.Type.Elem().Kind() == reflect.String {
			return &jsonSchema{Type: "array", Items: &jsonSchema{Type: "string"}}
		}
	case reflect.Map:
		if f.Type.Key().Kind() == reflect.String && f.Type.Elem().Kind() == reflect.String {
			return &jsonSchema{Type: "object", AdditionalProperties: &jsonSchema{Type: "string"}}
		}
	}
	panic(fmt.Sprintf("config: no JSON Schema mapping for op %s field %s of type %s", op, f.Name, f.Type))
}

// defaultValue renders a default in its JSON form (durations as strings).
func defaultValue(v reflect.Value) any {
	if d, ok := v.Interface().(model.Duration); ok {
		return d.String()
	}
	return v.Interface()
}

func tagName(tag, fieldName string) string {
	name, _, _ := strings.Cut(tag, ",")
	switch name {
	case "-":
		return "-"
	case "":
		return strings.ToLower(fieldName)
	}
	return name
}
