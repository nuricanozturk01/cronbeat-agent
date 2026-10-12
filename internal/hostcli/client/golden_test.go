package client

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func loadGolden(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "host_runs_contract.json"))
	if err != nil {
		t.Fatal(err)
	}
	var golden map[string]json.RawMessage
	if err := json.Unmarshal(data, &golden); err != nil {
		t.Fatal(err)
	}
	return golden
}

func withExit(req Request, code int, signal, output string) Request {
	req.Run.ExitCode = &code
	req.Run.Signal = signal
	req.Run.Output = output
	return req
}

func assertJSONEqual(t *testing.T, name string, got, want []byte) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("%s: golden entry missing or invalid: %v", name, err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("%s differs from testdata/host_runs_contract.json\n got  %s\n want %s", name, got, want)
	}
}
