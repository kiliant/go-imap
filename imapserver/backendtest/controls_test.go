package backendtest_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/kiliant/go-imap/imapserver/backendtest"
)

// Controls are public callback signatures, so adding options after v1 would
// break backend authors' adapters just as it would break an ordinary method.
func TestControlHooksTakeOptions(t *testing.T) {
	controls := reflect.TypeFor[backendtest.Controls]()
	for i := range controls.NumField() {
		field := controls.Field(i)
		if !field.IsExported() || field.Type.Kind() != reflect.Func {
			continue
		}
		callback := field.Type
		if callback.NumIn() < 2 || callback.In(0) != reflect.TypeFor[context.Context]() {
			t.Errorf("Controls.%s must take context.Context first and options last", field.Name)
			continue
		}
		options := callback.In(callback.NumIn() - 1)
		if options.Kind() != reflect.Pointer || options.Elem().Kind() != reflect.Struct ||
			options.Elem().PkgPath() != controls.PkgPath() ||
			!strings.HasSuffix(options.Elem().Name(), "Options") {
			t.Errorf("Controls.%s must end in a pointer to a backendtest options struct", field.Name)
		}
	}
}
