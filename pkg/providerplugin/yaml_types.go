package providerplugin

import (
	"encoding"
	"gopkg.in/yaml.v3"
	"reflect"
	"strings"
	"time"
)

var yamlUnmarshaler = reflect.TypeOf((*yaml.Unmarshaler)(nil)).Elem()
var textUnmarshaler = reflect.TypeOf((*encoding.TextUnmarshaler)(nil)).Elem()

// yaml.v3 intentionally coerces numbers and booleans into Go strings. Plugin
// configuration is a typed contract, so reject those implicit scalar changes.
// Custom decoders retain ownership of their own schema and validation.
func yamlTypes(n *yaml.Node, t reflect.Type) bool {
	if t == nil {
		return false
	}
	if t.Kind() == reflect.Pointer {
		if n.Tag == "!!null" {
			return true
		}
		return yamlTypes(n, t.Elem())
	}
	if reflect.PointerTo(t).Implements(yamlUnmarshaler) || reflect.PointerTo(t).Implements(textUnmarshaler) {
		return true
	}
	if t == reflect.TypeOf(time.Duration(0)) && n.Tag == "!!str" {
		return true
	}
	switch t.Kind() {
	case reflect.Interface:
		return true
	case reflect.String:
		return n.Kind == yaml.ScalarNode && n.Tag == "!!str"
	case reflect.Bool:
		return n.Kind == yaml.ScalarNode && n.Tag == "!!bool"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return n.Kind == yaml.ScalarNode && n.Tag == "!!int"
	case reflect.Float32, reflect.Float64:
		return n.Kind == yaml.ScalarNode && (n.Tag == "!!int" || n.Tag == "!!float")
	case reflect.Slice, reflect.Array:
		if n.Tag == "!!null" && t.Kind() == reflect.Slice {
			return true
		}
		if n.Kind != yaml.SequenceNode {
			return false
		}
		for _, v := range n.Content {
			if !yamlTypes(v, t.Elem()) {
				return false
			}
		}
	case reflect.Map:
		if n.Tag == "!!null" {
			return true
		}
		if n.Kind != yaml.MappingNode {
			return false
		}
		for i := 0; i < len(n.Content); i += 2 {
			if !yamlTypes(n.Content[i], t.Key()) || !yamlTypes(n.Content[i+1], t.Elem()) {
				return false
			}
		}
	case reflect.Struct:
		if t == reflect.TypeOf(yaml.Node{}) {
			return true
		}
		if n.Kind != yaml.MappingNode {
			return false
		}
		fields := map[string]*yaml.Node{}
		for i := 0; i < len(n.Content); i += 2 {
			fields[n.Content[i].Value] = n.Content[i+1]
		}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			tag := strings.Split(f.Tag.Get("yaml"), ",")
			name := tag[0]
			if name == "-" {
				continue
			}
			inline := false
			for _, option := range tag[1:] {
				if option == "inline" {
					inline = true
				}
			}
			if inline {
				if f.Type.Kind() != reflect.Map && !yamlTypes(n, f.Type) {
					return false
				}
				continue
			}
			if name == "" {
				name = strings.ToLower(f.Name)
			}
			if child := fields[name]; child != nil && !yamlTypes(child, f.Type) {
				return false
			}
		}
	}
	return true
}
