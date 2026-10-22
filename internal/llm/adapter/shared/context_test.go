package shared

import (
	"testing"

	"github.com/dillonzq/apix/internal/errclass"
)

func TestToolTransactionPreservesIdentityAndDiagnostics(t *testing.T) {
	for _, fail := range []bool{false, true} {
		ctx := NewConversionContext()
		ctx.Options.LossPolicy = LossPolicySafe
		if err := ctx.Tools.registerTool("existing", responseToolIdentity{name: "existing", isCustom: true}); err != nil {
			t.Fatal(err)
		}
		previous := DiagnosticFor(FormatResponses, FormatMessages, "metadata", false)
		added := DiagnosticFor(FormatResponses, FormatMessages, "input[0].unknown", true)
		ctx.Record(previous)
		_, err := ctx.WithToolTransaction(func(work *ConversionContext) ([]byte, *errclass.Error) {
			if work.Options != ctx.Options {
				t.Fatal("transaction lost request policy")
			}
			work.Record(added)
			if err := work.Tools.registerTool("new", responseToolIdentity{name: "new", isCustom: true}); err != nil {
				t.Fatal(err)
			}
			if ctx.IsCustom("new") {
				t.Fatal("uncommitted identity leaked")
			}
			if fail {
				return nil, errclass.Translation("rejected")
			}
			return []byte(`{}`), nil
		})
		if (err != nil) != fail || ctx.IsCustom("new") == fail || !ctx.IsCustom("existing") {
			t.Fatalf("fail=%v identities=%v error=%v", fail, ctx.Tools.names, err)
		}
		ctx.Record(previous)
		ctx.Record(added)
		diagnostics := ctx.Diagnostics()
		if len(diagnostics) != 2 || diagnostics[0] != previous || diagnostics[1] != added {
			t.Fatalf("diagnostics lost, duplicated or reordered: %v", diagnostics)
		}
		diagnostics[0].Path = "mutated"
		if ctx.Diagnostics()[0] != previous {
			t.Fatal("diagnostic copy changed context")
		}
	}
}
