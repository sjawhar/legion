package aws

import (
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/internal/bus"
)

// An nkey seed has no place in either component's inputs: each of them reaches a refusal's %q, a
// registered name, tag or alias, a resource's inputs, which a preview prints, or the rendered
// container definitions. So a seed (bus.HoldsSeed: any type, any spelling) is refused where it
// enters, naming the input by its path and never the value, as core and host:Listener refuse it:
// the component's name and every plain string input as the component resolves its inputs
// (seededPlainInput), and every Input once it resolves (refuseSeedInputs), so no child is
// registered with it. The secret ARNs are Inputs; an authorization's nkeys are plain inputs, and
// core refuses any nkey that is not a user public key without quoting it besides.

var (
	inputType            = reflect.TypeFor[pulumi.Input]()
	stringInputType      = reflect.TypeFor[pulumi.StringInput]()
	stringPtrInputType   = reflect.TypeFor[pulumi.StringPtrInput]()
	stringArrayInputType = reflect.TypeFor[pulumi.StringArrayInput]()
)

// refuseSeededName refuses a component name that holds an nkey seed: it reaches every child's name.
func refuseSeededName(typ, name string) error {
	if bus.HoldsSeed(name) {
		return fmt.Errorf("%s: %w", typ, core.SeedRefusal("the component's name"))
	}
	return nil
}

// seededPlainInput refuses the first plain string of args, a component's args struct, that holds
// an nkey seed, naming it by its schema path: fields by their pulumi names, slice elements and map
// values by index and key, and a map key by its map. Input fields are refuseSeedInputs'.
func seededPlainInput(args any) error {
	if path, ok := seedPath("", reflect.ValueOf(args)); ok {
		return core.SeedRefusal(path)
	}
	return nil
}

func seedPath(path string, v reflect.Value) (string, bool) {
	if v.Type().Implements(inputType) {
		return "", false
	}
	switch v.Kind() {
	case reflect.String:
		return path, bus.HoldsSeed(v.String())
	case reflect.Bool, reflect.Int:
		return "", false
	case reflect.Pointer:
		if v.IsNil() {
			return "", false
		}
		return seedPath(path, v.Elem())
	case reflect.Slice:
		for i := range v.Len() {
			if at, ok := seedPath(fmt.Sprintf("%s[%d]", path, i), v.Index(i)); ok {
				return at, true
			}
		}
		return "", false
	case reflect.Map:
		keys := v.MapKeys()
		slices.SortFunc(keys, func(a, b reflect.Value) int { return strings.Compare(a.String(), b.String()) })
		for _, key := range keys {
			if bus.HoldsSeed(key.String()) {
				return "a key of " + path, true
			}
			if at, ok := seedPath(fmt.Sprintf("%s[%s]", path, key.String()), v.MapIndex(key)); ok {
				return at, true
			}
		}
		return "", false
	case reflect.Struct:
		for i := range v.NumField() {
			name, _, _ := strings.Cut(v.Type().Field(i).Tag.Get("pulumi"), ",")
			if path != "" {
				name = path + "." + name
			}
			if at, ok := seedPath(name, v.Field(i)); ok {
				return at, true
			}
		}
		return "", false
	}
	panic(fmt.Sprintf("seedPath: an input of kind %s at %s, which no rule covers", v.Kind(), path))
}

// refuseSeedInputs replaces each Input field of args, a pointer to a component's args struct, with
// one that fails once it resolves to a value holding an nkey seed, naming the field by its path
// and never the value, so no child is registered with it. component prefixes the refusal.
func refuseSeedInputs(args any, component string) {
	v := reflect.ValueOf(args).Elem()
	for i := range v.NumField() {
		f := v.Field(i)
		input, _, _ := strings.Cut(v.Type().Field(i).Tag.Get("pulumi"), ",")
		refused := func(path string) error { return fmt.Errorf("%s: %w", component, core.SeedRefusal(path)) }
		switch {
		case f.Type() == stringInputType && !f.IsNil():
			f.Set(reflect.ValueOf(refuseSeedString(f.Interface().(pulumi.StringInput), func() error { return refused(input) })))
		case f.Type() == stringPtrInputType && !f.IsNil():
			f.Set(reflect.ValueOf(f.Interface().(pulumi.StringPtrInput).ToStringPtrOutput().ApplyT(func(s *string) (*string, error) {
				if s != nil && bus.HoldsSeed(*s) {
					return nil, refused(input)
				}
				return s, nil
			}).(pulumi.StringPtrOutput)))
		case f.Type() == stringArrayInputType && !f.IsNil():
			f.Set(reflect.ValueOf(f.Interface().(pulumi.StringArrayInput).ToStringArrayOutput().ApplyT(func(s []string) ([]string, error) {
				for j, e := range s {
					if bus.HoldsSeed(e) {
						return nil, refused(fmt.Sprintf("%s[%d]", input, j))
					}
				}
				return s, nil
			}).(pulumi.StringArrayOutput)))
		case f.Kind() == reflect.Slice && f.Type().Elem() == stringInputType:
			checked := make([]pulumi.StringInput, f.Len())
			for j := range checked {
				path := fmt.Sprintf("%s[%d]", input, j)
				checked[j] = refuseSeedString(f.Index(j).Interface().(pulumi.StringInput), func() error { return refused(path) })
			}
			f.Set(reflect.ValueOf(checked))
		case f.Type().Implements(inputType) && !f.IsNil(),
			f.Kind() == reflect.Slice && f.Type().Elem().Implements(inputType):
			panic(fmt.Sprintf("refuseSeedInputs: input %s is a %s, which no rule covers", input, f.Type()))
		}
	}
}

// refuseSeedString is in, failing with refused once it resolves to a value holding a seed.
func refuseSeedString(in pulumi.StringInput, refused func() error) pulumi.StringOutput {
	return in.ToStringOutput().ApplyT(func(s string) (string, error) {
		if bus.HoldsSeed(s) {
			return "", refused()
		}
		return s, nil
	}).(pulumi.StringOutput)
}
