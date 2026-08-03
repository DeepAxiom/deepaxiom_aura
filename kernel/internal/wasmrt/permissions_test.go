package wasmrt

import "testing"

func TestParsePermissionsDefaultsToFullyDenied(t *testing.T) {
	p, err := ParsePermissions(map[string]any{})
	if err != nil {
		t.Fatalf("ParsePermissions: %v", err)
	}
	if p.FSPath != "" || p.FSWritable || len(p.HTTPDomains) != 0 {
		t.Fatalf("p = %+v, want the fully-denied zero value", p)
	}
}

func TestParsePermissionsFilesystem(t *testing.T) {
	for _, tc := range []struct {
		name    string
		value   any
		wantErr bool
		path    string
		write   bool
	}{
		{name: "absent", value: nil},
		{name: "none", value: "none"},
		{name: "empty string", value: ""},
		{name: "read", value: "read:/data", path: "/data"},
		{name: "write", value: "write:/data", path: "/data", write: true},
		{name: "read with no path", value: "read:", wantErr: true},
		{name: "write with no path", value: "write:", wantErr: true},
		{name: "garbage", value: "delete-everything:/", wantErr: true},
		{name: "wrong type", value: 42, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := map[string]any{}
			if tc.value != nil {
				m["filesystem"] = tc.value
			}
			p, err := ParsePermissions(m)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("value %v: want an error, got p=%+v", tc.value, p)
				}
				return
			}
			if err != nil {
				t.Fatalf("value %v: ParsePermissions: %v", tc.value, err)
			}
			if p.FSPath != tc.path || p.FSWritable != tc.write {
				t.Fatalf("value %v: p=%+v, want path=%q write=%v", tc.value, p, tc.path, tc.write)
			}
		})
	}
}

func TestParsePermissionsHTTPDomains(t *testing.T) {
	for _, tc := range []struct {
		name    string
		value   any
		wantErr bool
		want    []string
	}{
		{name: "absent", value: nil, want: nil},
		{name: "empty list", value: []any{}, want: []string{}},
		{name: "one domain", value: []any{"api.example.com"}, want: []string{"api.example.com"}},
		{name: "several domains", value: []any{"a.com", "b.com"}, want: []string{"a.com", "b.com"}},
		{name: "not a list", value: "api.example.com", wantErr: true},
		{name: "non-string entry", value: []any{123}, wantErr: true},
		{name: "empty string entry", value: []any{""}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := map[string]any{}
			if tc.value != nil {
				m["egress_http"] = tc.value
			}
			p, err := ParsePermissions(m)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("value %v: want an error, got p=%+v", tc.value, p)
				}
				return
			}
			if err != nil {
				t.Fatalf("value %v: ParsePermissions: %v", tc.value, err)
			}
			if len(p.HTTPDomains) != len(tc.want) {
				t.Fatalf("value %v: HTTPDomains=%v, want %v", tc.value, p.HTTPDomains, tc.want)
			}
			for i := range tc.want {
				if p.HTTPDomains[i] != tc.want[i] {
					t.Fatalf("value %v: HTTPDomains=%v, want %v", tc.value, p.HTTPDomains, tc.want)
				}
			}
		})
	}
}
