package protocol

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bodyFor maps a fixture name prefix to a fresh body value.
func bodyFor(name string) Validator {
	switch name {
	case "hello":
		return &Hello{}
	case "hello_ack":
		return &HelloAck{}
	case "spec_show":
		return &SpecShow{}
	case "spec_show_result":
		return &SpecShowResult{}
	case "focus":
		return &Focus{}
	case "focus_result":
		return &FocusResult{}
	case "ask":
		return &Ask{}
	case "ask_shown":
		return &AskShown{}
	case "ask_result":
		return &AskResult{}
	case "ask_cancel":
		return &AskCancel{}
	case "bye":
		return &Bye{}
	case "done":
		return &Done{}
	case "error":
		return &ErrorBody{}
	case "ask_mode":
		return &AskMode{}
	case "ask_mode_result":
		return &AskModeResult{}
	case "doc_written":
		return &DocWritten{}
	case "doc_written_result":
		return &DocWrittenResult{}
	case "skill_started":
		return &SkillStarted{}
	case "doc_shown":
		return &DocShown{}
	case "session_cwd":
		return &SessionCwd{}
	case "cwd":
		return &Cwd{}
	case "cwd_result":
		return &CwdResult{}
	case "viewers":
		return &Viewers{}
	case "viewers_result":
		return &ViewersResult{}
	case "ask_here":
		return &AskHere{}
	case "ask_here_result":
		return &AskHereResult{}
	}
	return nil
}

// readFixture reads a fixture. The fixtures use macOS paths; on Windows
// they become C:/Users/…, so the absolute-path checks still apply.
func readFixture(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err == nil && runtime.GOOS == "windows" {
		data = bytes.ReplaceAll(data, []byte(`"/Users/`), []byte(`"C:/Users/`))
	}
	return data, err
}

func loadFixture(path string) (Envelope, Validator, error) {
	data, err := readFixture(path)
	if err != nil {
		return Envelope{}, nil, err
	}
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return env, nil, err
	}
	if err := env.Validate(); err != nil {
		return env, nil, err
	}
	body := bodyFor(strings.SplitN(filepath.Base(path), ".", 2)[0])
	if err := env.Decode(body); err != nil {
		return env, nil, err
	}
	return env, body, body.Validate()
}

func TestFixtures(t *testing.T) {
	files, err := filepath.Glob("../../protocol/fixtures/*.json")
	require.NoError(t, err)
	require.NotEmpty(t, files)

	seenValid := map[string]bool{}
	seenInvalid := map[string]bool{}
	for _, f := range files {
		base := filepath.Base(f)
		prefix := strings.SplitN(base, ".", 2)[0]
		require.NotNil(t, bodyFor(prefix), "unknown fixture prefix %s", base)
		valid := strings.Contains(base, ".valid.") || strings.HasSuffix(base, ".valid.json")
		t.Run(base, func(t *testing.T) {
			env, body, err := loadFixture(f)
			if !valid {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)

			// Round-trip: re-encode the body and the envelope and compare.
			raw, err := json.Marshal(body)
			require.NoError(t, err)
			assert.JSONEq(t, string(env.Body), string(raw))

			orig, err := readFixture(f)
			require.NoError(t, err)
			again, err := json.Marshal(env)
			require.NoError(t, err)
			assert.JSONEq(t, string(orig), string(again))
		})
		if valid {
			seenValid[prefix] = true
		} else {
			seenInvalid[prefix] = true
		}
	}
	for _, p := range []string{"hello", "hello_ack", "spec_show", "spec_show_result", "focus", "focus_result", "ask", "ask_shown", "ask_result", "ask_cancel", "bye", "done", "error", "viewers_result", "ask_mode", "doc_written", "skill_started", "doc_shown", "session_cwd", "cwd_result", "ask_here"} {
		assert.True(t, seenValid[p], "no valid fixture for %s", p)
		assert.True(t, seenInvalid[p], "no invalid fixture for %s", p)
	}
}
