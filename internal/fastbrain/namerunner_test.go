package fastbrain

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNameRunner(t *testing.T) {
	ctx := context.Background()
	got, err := NameRunner{NewEngine(fixed(`{"name":"auth-refactor"}`), nil)}.Run(ctx, "task")
	require.NoError(t, err)
	require.Equal(t, "auth-refactor", got)

	_, err = NameRunner{NewEngine(fixed("no json here"), nil)}.Run(ctx, "task")
	require.Error(t, err)
	_, err = NameRunner{NewEngine(nil, nil)}.Run(ctx, "task")
	require.Error(t, err, "no runner")
	_, err = NameRunner{NewEngine(RunnerFunc(func(context.Context, string) (string, error) { return "", errors.New("x") }), nil)}.Run(ctx, "task")
	require.Error(t, err)
	_, err = NameRunner{}.Run(ctx, "task")
	require.Error(t, err)
}
