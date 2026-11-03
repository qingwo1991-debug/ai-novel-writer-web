package publicresponse

import (
	"reflect"
	"sort"
	"strings"
	"unicode"
)

// Resolve fields before reading values, as encoding/json does. Untagged
// anonymous structs promote their fields without introducing a JSON level;
// shallow fields dominate deeper fields, tagged fields win at equal depth, and
// unresolved equal-depth collisions are omitted. No user methods are called.
type jsonField struct {
	name    string
	index   []int
	tagged  bool
	options []string
}

func validJSONTag(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if strings.ContainsRune("!#$%&()*+-./:;<=>?@[]^_{|}~ ", r) {
			continue
		}
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

func jsonFields(typ reflect.Type) []jsonField {
	candidates := make(map[string][]jsonField)
	active := make(map[reflect.Type]bool)
	var collect func(reflect.Type, []int)
	collect = func(current reflect.Type, prefix []int) {
		if active[current] {
			return
		}
		active[current] = true
		defer delete(active, current)
		for i := 0; i < current.NumField(); i++ {
			field := current.Field(i)
			underlying := field.Type
			if underlying.Kind() == reflect.Pointer {
				underlying = underlying.Elem()
			}
			if field.PkgPath != "" && (!field.Anonymous || underlying.Kind() != reflect.Struct) {
				continue
			}
			tag := strings.Split(field.Tag.Get("json"), ",")
			if field.Tag.Get("json") == "-" {
				continue
			}
			name := tag[0]
			if !validJSONTag(name) {
				name = ""
			}
			index := append(append([]int(nil), prefix...), i)
			if name == "" && field.Anonymous && underlying.Kind() == reflect.Struct {
				collect(underlying, index)
				continue
			}
			tagged := name != ""
			if name == "" {
				name = field.Name
			}
			candidates[name] = append(candidates[name], jsonField{name: name, index: index, tagged: tagged, options: tag[1:]})
		}
	}
	collect(typ, nil)
	fields := make([]jsonField, 0, len(candidates))
	for _, group := range candidates {
		minimum := len(group[0].index)
		for _, field := range group[1:] {
			if len(field.index) < minimum {
				minimum = len(field.index)
			}
		}
		var winner jsonField
		count, taggedCount := 0, 0
		for _, field := range group {
			if len(field.index) != minimum {
				continue
			}
			count++
			if field.tagged {
				taggedCount++
				winner = field
			} else if taggedCount == 0 {
				winner = field
			}
		}
		if taggedCount == 1 || (taggedCount == 0 && count == 1) {
			fields = append(fields, winner)
		}
	}
	sort.Slice(fields, func(i, j int) bool {
		a, b := fields[i].index, fields[j].index
		for k := 0; k < len(a) && k < len(b); k++ {
			if a[k] != b[k] {
				return a[k] < b[k]
			}
		}
		return len(a) < len(b)
	})
	return fields
}

func indexedField(value reflect.Value, index []int) (reflect.Value, bool) {
	for _, i := range index {
		if value.Kind() == reflect.Pointer {
			if value.IsNil() {
				return reflect.Value{}, false
			}
			value = value.Elem()
		}
		value = value.Field(i)
	}
	return value, true
}

func (c *clone) structure(value reflect.Value, depth int) map[string]any {
	out := make(map[string]any)
	for _, field := range jsonFields(value.Type()) {
		item, exists := indexedField(value, field.index)
		if !exists {
			continue
		}
		omit := false
		for _, option := range field.options {
			if option == "omitempty" && emptyValue(item) {
				omit = true
			}
			if option == "omitzero" && item.IsZero() {
				omit = true
			}
		}
		if !omit {
			c.put(out, field.name, item, depth+1)
		}
	}
	return out
}
