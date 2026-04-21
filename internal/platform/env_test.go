package platform

import (
	"reflect"
	"testing"
)

func TestWithoutLoopbackProxies(t *testing.T) {
	tests := []struct {
		name string
		env  []string
		want []string
	}{
		{
			name: "empty env",
			env:  []string{},
			want: []string{},
		},
		{
			name: "no proxy vars",
			env:  []string{"PATH=/usr/bin", "HOME=/home/user"},
			want: []string{"PATH=/usr/bin", "HOME=/home/user"},
		},
		{
			name: "strips loopback HTTP_PROXY with scheme",
			env:  []string{"HTTP_PROXY=http://127.0.0.1:64788", "PATH=/usr/bin"},
			want: []string{"PATH=/usr/bin"},
		},
		{
			name: "strips lowercase https_proxy",
			env:  []string{"https_proxy=http://127.0.0.1:8080"},
			want: []string{},
		},
		{
			name: "strips mixed-case Http_Proxy (Windows preserved casing)",
			env:  []string{"Http_Proxy=http://127.0.0.1:64788"},
			want: []string{},
		},
		{
			name: "strips mixed-case HTTP_proxy",
			env:  []string{"HTTP_proxy=http://127.0.0.1:64788"},
			want: []string{},
		},
		{
			name: "keeps mixed-case proxy pointing at corp host",
			env:  []string{"Https_Proxy=http://proxy.corp.internal:8080"},
			want: []string{"Https_Proxy=http://proxy.corp.internal:8080"},
		},
		{
			name: "strips scheme-less loopback value",
			env:  []string{"HTTPS_PROXY=127.0.0.1:64788"},
			want: []string{},
		},
		{
			name: "strips localhost hostname",
			env:  []string{"HTTP_PROXY=http://localhost:8080"},
			want: []string{},
		},
		{
			name: "strips localhost scheme-less",
			env:  []string{"HTTP_PROXY=localhost:8080"},
			want: []string{},
		},
		{
			name: "strips IPv6 loopback",
			env:  []string{"HTTP_PROXY=http://[::1]:8080"},
			want: []string{},
		},
		{
			name: "strips 127.x.x.x beyond 127.0.0.1",
			env:  []string{"HTTP_PROXY=http://127.1.2.3:8080"},
			want: []string{},
		},
		{
			name: "strips socks5 scheme",
			env:  []string{"ALL_PROXY=socks5://127.0.0.1:1080"},
			want: []string{},
		},
		{
			name: "strips socks5h scheme",
			env:  []string{"ALL_PROXY=socks5h://127.0.0.1:1080"},
			want: []string{},
		},
		{
			name: "strips loopback with userinfo",
			env:  []string{"HTTP_PROXY=http://user:pass@127.0.0.1:8080"},
			want: []string{},
		},
		{
			name: "keeps corp proxy hostname",
			env:  []string{"HTTPS_PROXY=http://proxy.corp.internal:8080"},
			want: []string{"HTTPS_PROXY=http://proxy.corp.internal:8080"},
		},
		{
			name: "keeps non-loopback private IP",
			env:  []string{"HTTPS_PROXY=http://10.0.0.1:8080"},
			want: []string{"HTTPS_PROXY=http://10.0.0.1:8080"},
		},
		{
			name: "keeps NO_PROXY even with loopback entries",
			env:  []string{"NO_PROXY=127.0.0.1,localhost"},
			want: []string{"NO_PROXY=127.0.0.1,localhost"},
		},
		{
			name: "keeps empty proxy value",
			env:  []string{"HTTPS_PROXY="},
			want: []string{"HTTPS_PROXY="},
		},
		{
			name: "keeps unparseable value",
			env:  []string{"HTTPS_PROXY=:::not a url:::"},
			want: []string{"HTTPS_PROXY=:::not a url:::"},
		},
		{
			name: "keeps malformed entry without equals",
			env:  []string{"MALFORMED"},
			want: []string{"MALFORMED"},
		},
		{
			name: "mixed: strip loopback, keep corp",
			env: []string{
				"HTTP_PROXY=http://127.0.0.1:64788",
				"HTTPS_PROXY=http://proxy.corp.internal:8080",
				"PATH=/usr/bin",
			},
			want: []string{
				"HTTPS_PROXY=http://proxy.corp.internal:8080",
				"PATH=/usr/bin",
			},
		},
		{
			name: "preserves order of retained entries",
			env: []string{
				"A=1",
				"HTTP_PROXY=http://127.0.0.1:1",
				"B=2",
				"https_proxy=127.0.0.1:2",
				"C=3",
			},
			want: []string{"A=1", "B=2", "C=3"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := WithoutLoopbackProxies(tt.env)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("WithoutLoopbackProxies(%v) = %v, want %v", tt.env, got, tt.want)
			}
		})
	}
}

func TestWithoutLoopbackProxies_DoesNotMutateInput(t *testing.T) {
	env := []string{
		"HTTP_PROXY=http://127.0.0.1:64788",
		"PATH=/usr/bin",
	}
	snapshot := append([]string(nil), env...)
	_ = WithoutLoopbackProxies(env)
	if !reflect.DeepEqual(env, snapshot) {
		t.Errorf("input was mutated: got %v, want %v", env, snapshot)
	}
}
