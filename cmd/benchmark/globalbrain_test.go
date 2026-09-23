// Tests for the Global Brain flags: without --plane the scenario is skipped
// entirely, and with it every way the configuration can be wrong fails before a
// request is made -- naming the flag, never the credential.
package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testAPIKey stands in for the tenant credential. Nothing may print it.
const testAPIKey = "test-tenant-jwt-value"

func TestGlobalBrainOptionsSkippedWithoutAPlane(t *testing.T) {
	opts, err := globalBrainOptionsFromFlags("", "", "agent_a", "agent_b", "", 5)

	require.NoError(t, err)
	assert.False(t, opts.enabled(), "a run without --plane must stay local")
}

func TestGlobalBrainOptionsRejectACredentialWithoutAPlane(t *testing.T) {
	_, err := globalBrainOptionsFromFlags("", testAPIKey, "agent_a", "agent_b", "", 5)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--api-key")
	assert.NotContains(t, err.Error(), testAPIKey, "an error must never echo the credential")
}

func TestGlobalBrainOptionsRequireACredentialWithAPlane(t *testing.T) {
	_, err := globalBrainOptionsFromFlags("http://127.0.0.1:9090", "", "agent_a", "agent_b", "", 5)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--api-key")
}

func TestGlobalBrainOptionsDefaultToDistilled(t *testing.T) {
	opts, err := globalBrainOptionsFromFlags("http://127.0.0.1:9090", testAPIKey, "agent_a", "agent_b", "", 5)

	require.NoError(t, err)
	require.True(t, opts.enabled())
	assert.Equal(t, globalBrainDistilled, opts.Mode)
	assert.Equal(t, 5, opts.Sessions)
	assert.Equal(t, "agent_a", opts.AgentA)
	assert.Equal(t, "agent_b", opts.AgentB)
}

func TestGlobalBrainOptionsAcceptFullMode(t *testing.T) {
	opts, err := globalBrainOptionsFromFlags("http://127.0.0.1:9090", testAPIKey, "a", "b", globalBrainFull, 1)

	require.NoError(t, err)
	assert.Equal(t, globalBrainFull, opts.Mode)
}

func TestGlobalBrainOptionsRejectUnknownMode(t *testing.T) {
	_, err := globalBrainOptionsFromFlags("http://127.0.0.1:9090", testAPIKey, "a", "b", "sampled", 5)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--global-brain")
}

func TestGlobalBrainOptionsRequireTwoDistinctAgents(t *testing.T) {
	_, err := globalBrainOptionsFromFlags("http://127.0.0.1:9090", testAPIKey, "agent_a", "agent_a", "", 5)
	require.Error(t, err, "one agent reading its own memory is not the scenario")

	_, err = globalBrainOptionsFromFlags("http://127.0.0.1:9090", testAPIKey, "", "agent_b", "", 5)
	require.Error(t, err)
}

func TestGlobalBrainOptionsRequireAtLeastOneSession(t *testing.T) {
	_, err := globalBrainOptionsFromFlags("http://127.0.0.1:9090", testAPIKey, "agent_a", "agent_b", "", 0)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--global-sessions")
}
