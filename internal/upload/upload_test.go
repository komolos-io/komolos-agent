package upload

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/komolos-io/komolos-agent/internal/inventory"
)

func doc(t *testing.T) []byte {
	b, err := inventory.Marshal(&inventory.Inventory{
		SchemaVersion: inventory.SchemaVersion, Provider: "aws", CollectedAt: time.Now().UTC(),
		AWS: &inventory.AWS{AccountID: "1"}, Gaps: []inventory.Gap{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSend(t *testing.T) {
	d := doc(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != Path || r.Header.Get("Authorization") != "Bearer kup_abc" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		b, _ := io.ReadAll(r.Body)
		json.NewEncoder(w).Encode(Result{InventoryID: "inv-1", SHA256: inventory.SHA256(b)})
	}))
	defer srv.Close()
	ep := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)

	res, err := Send(context.Background(), ep, " kup_abc\n", d)
	if err != nil || res.InventoryID != "inv-1" {
		t.Fatalf("Send: %v %+v", err, res)
	}
	if _, err := Send(context.Background(), ep, "ghp_wrongkind", d); err == nil {
		t.Error("accepted a token of the wrong kind")
	}
	if _, err := Send(context.Background(), "http://example.com", "kup_abc", d); err == nil {
		t.Error("accepted plain http to a remote host")
	}
	if _, err := Send(context.Background(), ep, "kup_abc", []byte(`{"schema_version":"0.2","provider":"aws","extra":1}`)); err == nil {
		t.Error("uploaded an invalid document")
	}
}
