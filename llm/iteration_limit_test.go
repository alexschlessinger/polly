package llm

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestIterationLimitClassificationKeepsMixedFailures(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{nil, false},
		{ErrMaxIterations, true},
		{fmt.Errorf("wrapped: %w", ErrMaxIterations), true},
		{errors.Join(ErrMaxIterations, ErrMaxIterations), true},
		{errors.Join(ErrMaxIterations, errors.New("checkpoint failed")), false},
		{errors.Join(ErrMaxIterations, context.Canceled), false},
		{errors.New(ErrMaxIterations.Error()), false},
	} {
		if got := IsIterationLimit(tc.err); got != tc.want {
			t.Fatalf("%v: got %t, want %t", tc.err, got, tc.want)
		}
	}
}
