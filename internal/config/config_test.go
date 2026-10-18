/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var completeEnv = map[string]string{
	"OMADA_SERVER":        "https://omada.example.com:8043",
	"OMADA_ID":            "OM-12345678",
	"OMADA_CLIENT_ID":     "client-id",
	"OMADA_CLIENT_SECRET": "secret",
	"OMADA_SITE_NAME":     "Home",
}

func parseWithEnv(t *testing.T, env map[string]string, args ...string) (*Config, error) {
	t.Helper()
	for k, v := range env {
		t.Setenv(k, v)
	}
	c, err := Parse(args)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func TestParseDefaults(t *testing.T) {
	c, err := parseWithEnv(t, completeEnv)
	require.NoError(t, err)

	assert.Equal(t, completeEnv["OMADA_SERVER"], c.Omada.Server)
	assert.Equal(t, completeEnv["OMADA_ID"], c.Omada.ID)
	assert.Equal(t, completeEnv["OMADA_CLIENT_ID"], c.Omada.ClientID)
	assert.Equal(t, completeEnv["OMADA_CLIENT_SECRET"], c.Omada.ClientSecret)
	assert.Equal(t, completeEnv["OMADA_SITE_NAME"], c.Omada.SiteName)
	assert.Equal(t, "127.0.0.1:8888", c.WebhookListen)
	assert.Equal(t, "0.0.0.0:8080", c.MetricsListen)
	assert.Equal(t, 30*time.Second, c.ReadTimeout)
	assert.Equal(t, 60*time.Second, c.WriteTimeout)
	assert.Equal(t, int64(32<<20), c.MaxBodySize)
	assert.Equal(t, "info", c.LogLevel)
	assert.Equal(t, "logfmt", c.LogFormat)
	assert.False(t, c.Omada.DryRun)
	assert.False(t, c.Omada.TLSInsecureSkipVerify)
	assert.Empty(t, c.Omada.Networks)
	assert.Empty(t, c.Omada.DomainFilter)
}

func TestParseFlagsOverrideEnv(t *testing.T) {
	c, err := parseWithEnv(t, completeEnv,
		"--omada-site-name=Other",
		"--omada-networks=vlan10, vlan20",
		"--domain-filter=lan, .home.lan",
		"--webhook-listen=127.0.0.1:9999",
		"--dry-run",
		"--omada-tls-skip-verify",
	)
	require.NoError(t, err)

	assert.Equal(t, "Other", c.Omada.SiteName)
	assert.Equal(t, []string{"vlan10", "vlan20"}, splitList(c.Omada.Networks))
	assert.Equal(t, []string{"lan", ".home.lan"}, c.Omada.DomainFilter)
	assert.Equal(t, "127.0.0.1:9999", c.WebhookListen)
	assert.True(t, c.Omada.DryRun)
	assert.True(t, c.Omada.TLSInsecureSkipVerify)
	// Unset flags keep the env values.
	assert.Equal(t, completeEnv["OMADA_SERVER"], c.Omada.Server)
}

func TestParseEnvOnly(t *testing.T) {
	env := cloneEnv(completeEnv)
	env["DRY_RUN"] = "true"
	env["OMADA_NETWORKS"] = " guest , default "
	env["WEBHOOK_READ_TIMEOUT"] = "5m"
	env["MAX_BODY_SIZE"] = "1048576"
	c, err := parseWithEnv(t, env)
	require.NoError(t, err)

	assert.True(t, c.Omada.DryRun)
	assert.Equal(t, []string{"guest", "default"}, splitList(c.Omada.Networks))
	assert.Equal(t, 5*time.Minute, c.ReadTimeout)
	assert.Equal(t, int64(1048576), c.MaxBodySize)
}

func TestParseInvalidEnvValuesFallBackToDefaults(t *testing.T) {
	env := cloneEnv(completeEnv)
	env["DRY_RUN"] = "not-a-bool"
	env["WEBHOOK_READ_TIMEOUT"] = "not-a-duration"
	env["MAX_BODY_SIZE"] = "not-a-number"
	c, err := parseWithEnv(t, env)
	require.NoError(t, err)

	assert.False(t, c.Omada.DryRun)
	assert.Equal(t, 30*time.Second, c.ReadTimeout)
	assert.Equal(t, int64(32<<20), c.MaxBodySize)
}

func TestParseInvalidFlagValues(t *testing.T) {
	for _, args := range [][]string{
		{"--webhook-read-timeout=soon"},
		{"--max-body-size=huge"},
		{"--dry-run=not-a-bool"},
		{"--unknown-flag"},
	} {
		_, err := parseWithEnv(t, completeEnv, args...)
		require.Error(t, err, "args %v", args)
	}
}

func TestParseRequiredFields(t *testing.T) {
	_, err := parseWithEnv(t, map[string]string{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "omada-server")

	for key, flag := range map[string]string{
		"OMADA_SERVER":        "omada-server",
		"OMADA_ID":            "omada-id",
		"OMADA_CLIENT_ID":     "omada-client-id",
		"OMADA_CLIENT_SECRET": "omada-client-secret",
		"OMADA_SITE_NAME":     "omada-site-name",
	} {
		t.Run(key, func(t *testing.T) {
			env := cloneEnv(completeEnv)
			delete(env, key)
			_, err := parseWithEnv(t, env)
			require.Error(t, err)
			assert.Contains(t, err.Error(), flag)
		})
	}
}

func TestSplitList(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"lan", []string{"lan"}},
		{"lan, home.lan", []string{"lan", "home.lan"}},
		{"a,,b, ,c", []string{"a", "b", "c"}},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, splitList(tc.in))
		})
	}
}

func cloneEnv(env map[string]string) map[string]string {
	out := make(map[string]string, len(env))
	for k, v := range env {
		out[k] = v
	}
	return out
}
