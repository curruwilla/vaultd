package config

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"
)

// placeholder matches ${VAR}, ${VAR:-default} and ${file:/path/to/secret}.
var placeholder = regexp.MustCompile(`\$\{([^}]*)\}`)

// interpolator resolves ${...} placeholders. Lookup and ReadFile are fields so
// tests can drive it without touching the real environment or filesystem.
type interpolator struct {
	Lookup   func(string) (string, bool)
	ReadFile func(string) ([]byte, error)
	// AllowUnset downgrades an unresolvable placeholder to a warning and
	// substitutes an empty string. It exists so CI can validate a config
	// without holding production secrets.
	AllowUnset bool

	diags Diagnostics
}

func newInterpolator() *interpolator {
	return &interpolator{Lookup: os.LookupEnv, ReadFile: os.ReadFile}
}

// interpolate walks every string in v (which must be a pointer to a struct)
// and expands the placeholders it finds in place. Values come from the
// environment, never from the YAML itself, so no secret is ever written down
// in the config file (SPEC §9).
func (in *interpolator) interpolate(v any) Diagnostics {
	in.diags = nil
	in.walk(reflect.ValueOf(v), "")
	return in.diags
}

func (in *interpolator) walk(v reflect.Value, path string) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			in.walk(v.Elem(), path)
		}

	case reflect.Struct:
		t := v.Type()
		for i := range v.NumField() {
			field := t.Field(i)
			name := fieldName(field)
			if !field.IsExported() || name == "-" {
				continue
			}
			in.walk(v.Field(i), join(path, name))
		}

	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			in.walk(v.Index(i), fmt.Sprintf("%s[%d]", path, i))
		}

	case reflect.Map:
		for _, key := range v.MapKeys() {
			elem := v.MapIndex(key)
			if elem.Kind() != reflect.String {
				// Map elements are not addressable; only plain string maps
				// can be rewritten, and those are all the config has.
				continue
			}
			expanded := in.expand(elem.String(), fmt.Sprintf("%s[%v]", path, key.Interface()))
			out := reflect.New(elem.Type()).Elem()
			out.SetString(expanded)
			v.SetMapIndex(key, out)
		}

	case reflect.String:
		if !v.CanSet() {
			return
		}
		v.SetString(in.expand(v.String(), path))
	}
}

func (in *interpolator) expand(s, path string) string {
	if !strings.Contains(s, "${") {
		return s
	}

	return placeholder.ReplaceAllStringFunc(s, func(match string) string {
		ref := placeholder.FindStringSubmatch(match)[1]
		value, err := in.resolve(ref)
		if err == nil {
			return value
		}

		if in.AllowUnset {
			in.diags.warnf(path, "%s: %s (substituted with an empty string)", path, err)
			return ""
		}
		in.diags.errorf(path, "%s: %s", path, err)
		return match
	})
}

func (in *interpolator) resolve(ref string) (string, error) {
	if file, ok := strings.CutPrefix(ref, "file:"); ok {
		file = strings.TrimSpace(file)
		if file == "" {
			return "", errors.New("${file:} needs a path")
		}
		b, err := in.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("${file:%s} cannot be read: %w", file, err)
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}

	name, fallback, hasFallback := strings.Cut(ref, ":-")
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("${%s} is not a variable reference", ref)
	}

	if value, ok := in.Lookup(name); ok {
		return value, nil
	}
	if hasFallback {
		return fallback, nil
	}
	return "", fmt.Errorf("${%s} is not set in the environment", name)
}

// fieldName returns the YAML name of a struct field, so diagnostic paths match
// what the user actually typed.
func fieldName(f reflect.StructField) string {
	tag, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
	if tag == "" {
		return strings.ToLower(f.Name)
	}
	return tag
}

func join(path, field string) string {
	if path == "" {
		return field
	}
	return path + "." + field
}
