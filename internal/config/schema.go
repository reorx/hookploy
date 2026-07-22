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

// stepSchema builds the two step forms from the op catalog: a bare string
// (ops whose zero-value Args validate) and a single-key map (op name → args).
func stepSchema() *jsonSchema {
	var zeroArgOps []any
	argMaps := map[string]*jsonSchema{}
	for _, info := range ops.Catalog() {
		if info.Defaults.Validate() == nil {
			zeroArgOps = append(zeroArgOps, info.Name)
		}
		argMaps[info.Name] = opArgsSchema(info)
	}
	return &jsonSchema{
		Description: "流水线的一步：字符串 = 无参 op，单键 map = op 名 → 参数。",
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
