package fbc_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/operator-framework/operator-controller/internal/shared/fbc"
)

func TestLifecyclePhase_Compare(t *testing.T) {
	// Create extension phases for testing
	extension1 := fbc.LifecycleExtensionPhase(1)
	extension2 := fbc.LifecycleExtensionPhase(2)
	extension3 := fbc.LifecycleExtensionPhase(3)

	tests := []struct {
		name     string
		a        fbc.LifecyclePhase
		b        fbc.LifecyclePhase
		expected int
	}{
		// Same phase comparisons
		{
			name:     "FullSupport == FullSupport",
			a:        fbc.LifecyclePhaseFullSupport,
			b:        fbc.LifecyclePhaseFullSupport,
			expected: 0,
		},
		{
			name:     "Maintenance == Maintenance",
			a:        fbc.LifecyclePhaseMaintenance,
			b:        fbc.LifecyclePhaseMaintenance,
			expected: 0,
		},
		{
			name:     "EndOfLife == EndOfLife",
			a:        fbc.LifecyclePhaseEndOfLife,
			b:        fbc.LifecyclePhaseEndOfLife,
			expected: 0,
		},
		{
			name:     "PreGA == PreGA",
			a:        fbc.LifecyclePhasePreGA,
			b:        fbc.LifecyclePhasePreGA,
			expected: 0,
		},
		{
			name:     "Extension1 == Extension1",
			a:        extension1,
			b:        extension1,
			expected: 0,
		},

		// FullSupport comparisons (FullSupport is the "best" phase, so it should be > others)
		{
			name:     "FullSupport > Maintenance",
			a:        fbc.LifecyclePhaseFullSupport,
			b:        fbc.LifecyclePhaseMaintenance,
			expected: 1,
		},
		{
			name:     "FullSupport > Extension1",
			a:        fbc.LifecyclePhaseFullSupport,
			b:        extension1,
			expected: 1,
		},
		{
			name:     "FullSupport > Extension2",
			a:        fbc.LifecyclePhaseFullSupport,
			b:        extension2,
			expected: 1,
		},
		{
			name:     "FullSupport > EndOfLife",
			a:        fbc.LifecyclePhaseFullSupport,
			b:        fbc.LifecyclePhaseEndOfLife,
			expected: 1,
		},
		{
			name:     "FullSupport > PreGA",
			a:        fbc.LifecyclePhaseFullSupport,
			b:        fbc.LifecyclePhasePreGA,
			expected: 1,
		},

		// Maintenance comparisons
		{
			name:     "Maintenance < FullSupport",
			a:        fbc.LifecyclePhaseMaintenance,
			b:        fbc.LifecyclePhaseFullSupport,
			expected: -1,
		},
		{
			name:     "Maintenance > Extension1",
			a:        fbc.LifecyclePhaseMaintenance,
			b:        extension1,
			expected: 1,
		},
		{
			name:     "Maintenance > Extension2",
			a:        fbc.LifecyclePhaseMaintenance,
			b:        extension2,
			expected: 1,
		},
		{
			name:     "Maintenance > EndOfLife",
			a:        fbc.LifecyclePhaseMaintenance,
			b:        fbc.LifecyclePhaseEndOfLife,
			expected: 1,
		},
		{
			name:     "Maintenance > PreGA",
			a:        fbc.LifecyclePhaseMaintenance,
			b:        fbc.LifecyclePhasePreGA,
			expected: 1,
		},

		// Extension1 comparisons
		{
			name:     "Extension1 < FullSupport",
			a:        extension1,
			b:        fbc.LifecyclePhaseFullSupport,
			expected: -1,
		},
		{
			name:     "Extension1 < Maintenance",
			a:        extension1,
			b:        fbc.LifecyclePhaseMaintenance,
			expected: -1,
		},
		{
			name:     "Extension1 > Extension2",
			a:        extension1,
			b:        extension2,
			expected: 1,
		},
		{
			name:     "Extension1 > Extension3",
			a:        extension1,
			b:        extension3,
			expected: 1,
		},
		{
			name:     "Extension1 > EndOfLife",
			a:        extension1,
			b:        fbc.LifecyclePhaseEndOfLife,
			expected: 1,
		},
		{
			name:     "Extension1 > PreGA",
			a:        extension1,
			b:        fbc.LifecyclePhasePreGA,
			expected: 1,
		},

		// Extension2 comparisons
		{
			name:     "Extension2 < FullSupport",
			a:        extension2,
			b:        fbc.LifecyclePhaseFullSupport,
			expected: -1,
		},
		{
			name:     "Extension2 < Maintenance",
			a:        extension2,
			b:        fbc.LifecyclePhaseMaintenance,
			expected: -1,
		},
		{
			name:     "Extension2 < Extension1",
			a:        extension2,
			b:        extension1,
			expected: -1,
		},
		{
			name:     "Extension2 > Extension3",
			a:        extension2,
			b:        extension3,
			expected: 1,
		},
		{
			name:     "Extension2 > EndOfLife",
			a:        extension2,
			b:        fbc.LifecyclePhaseEndOfLife,
			expected: 1,
		},
		{
			name:     "Extension2 > PreGA",
			a:        extension2,
			b:        fbc.LifecyclePhasePreGA,
			expected: 1,
		},

		// Extension3 comparisons
		{
			name:     "Extension3 < FullSupport",
			a:        extension3,
			b:        fbc.LifecyclePhaseFullSupport,
			expected: -1,
		},
		{
			name:     "Extension3 < Maintenance",
			a:        extension3,
			b:        fbc.LifecyclePhaseMaintenance,
			expected: -1,
		},
		{
			name:     "Extension3 < Extension1",
			a:        extension3,
			b:        extension1,
			expected: -1,
		},
		{
			name:     "Extension3 < Extension2",
			a:        extension3,
			b:        extension2,
			expected: -1,
		},
		{
			name:     "Extension3 > EndOfLife",
			a:        extension3,
			b:        fbc.LifecyclePhaseEndOfLife,
			expected: 1,
		},
		{
			name:     "Extension3 > PreGA",
			a:        extension3,
			b:        fbc.LifecyclePhasePreGA,
			expected: 1,
		},

		// EndOfLife comparisons (EndOfLife is worse than most phases)
		{
			name:     "EndOfLife < FullSupport",
			a:        fbc.LifecyclePhaseEndOfLife,
			b:        fbc.LifecyclePhaseFullSupport,
			expected: -1,
		},
		{
			name:     "EndOfLife < Maintenance",
			a:        fbc.LifecyclePhaseEndOfLife,
			b:        fbc.LifecyclePhaseMaintenance,
			expected: -1,
		},
		{
			name:     "EndOfLife < Extension1",
			a:        fbc.LifecyclePhaseEndOfLife,
			b:        extension1,
			expected: -1,
		},
		{
			name:     "EndOfLife < Extension2",
			a:        fbc.LifecyclePhaseEndOfLife,
			b:        extension2,
			expected: -1,
		},
		{
			name:     "EndOfLife > PreGA",
			a:        fbc.LifecyclePhaseEndOfLife,
			b:        fbc.LifecyclePhasePreGA,
			expected: 1,
		},

		// PreGA comparisons (PreGA is the "worst" phase)
		{
			name:     "PreGA < FullSupport",
			a:        fbc.LifecyclePhasePreGA,
			b:        fbc.LifecyclePhaseFullSupport,
			expected: -1,
		},
		{
			name:     "PreGA < Maintenance",
			a:        fbc.LifecyclePhasePreGA,
			b:        fbc.LifecyclePhaseMaintenance,
			expected: -1,
		},
		{
			name:     "PreGA < Extension1",
			a:        fbc.LifecyclePhasePreGA,
			b:        extension1,
			expected: -1,
		},
		{
			name:     "PreGA < Extension2",
			a:        fbc.LifecyclePhasePreGA,
			b:        extension2,
			expected: -1,
		},
		{
			name:     "PreGA < EndOfLife",
			a:        fbc.LifecyclePhasePreGA,
			b:        fbc.LifecyclePhaseEndOfLife,
			expected: -1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			actual := test.a.Compare(test.b)
			assert.Equal(t, test.expected, actual, "Compare(%v, %v)", test.a, test.b)
		})
	}
}

func TestLifecycleExtensionPhase(t *testing.T) {
	tests := []struct {
		name     string
		input    int
		expected fbc.LifecyclePhase
	}{
		{
			name:     "Extension phase 1",
			input:    1,
			expected: fbc.LifecyclePhase(2), // i + 1
		},
		{
			name:     "Extension phase 2",
			input:    2,
			expected: fbc.LifecyclePhase(3), // i + 1
		},
		{
			name:     "Extension phase 10",
			input:    10,
			expected: fbc.LifecyclePhase(11), // i + 1
		},
		{
			name:     "Extension phase 100",
			input:    100,
			expected: fbc.LifecyclePhase(101), // i + 1
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			actual := fbc.LifecycleExtensionPhase(test.input)
			assert.Equal(t, test.expected, actual)

			// Verify the string representation follows the expected format
			expectedStr := fmt.Sprintf("EUS-%d", test.input)
			assert.Equal(t, expectedStr, actual.String())
		})
	}
}

func TestLifecycleExtensionPhase_Panics(t *testing.T) {
	panicTests := []struct {
		name  string
		input int
	}{
		{
			name:  "Input 0 should panic",
			input: 0,
		},
		{
			name:  "Negative input should panic",
			input: -1,
		},
		{
			name:  "Input at EndOfLife-1 boundary should panic",
			input: int(fbc.LifecyclePhaseEndOfLife - 1),
		},
		{
			name:  "Input above EndOfLife-1 boundary should panic",
			input: int(fbc.LifecyclePhaseEndOfLife),
		},
	}

	for _, test := range panicTests {
		t.Run(test.name, func(t *testing.T) {
			assert.Panics(t, func() {
				fbc.LifecycleExtensionPhase(test.input)
			}, "Expected panic for input %d", test.input)
		})
	}
}

func TestLifecyclePhase_String(t *testing.T) {
	tests := []struct {
		name     string
		phase    fbc.LifecyclePhase
		expected string
	}{
		{
			name:     "PreGA string representation",
			phase:    fbc.LifecyclePhasePreGA,
			expected: "Pre-GA",
		},
		{
			name:     "FullSupport string representation",
			phase:    fbc.LifecyclePhaseFullSupport,
			expected: "Full Support",
		},
		{
			name:     "Maintenance string representation",
			phase:    fbc.LifecyclePhaseMaintenance,
			expected: "Maintenance",
		},
		{
			name:     "EndOfLife string representation",
			phase:    fbc.LifecyclePhaseEndOfLife,
			expected: "End of Life",
		},
		{
			name:     "Extension phase 1 string representation",
			phase:    fbc.LifecycleExtensionPhase(1),
			expected: "EUS-1",
		},
		{
			name:     "Extension phase 2 string representation",
			phase:    fbc.LifecycleExtensionPhase(2),
			expected: "EUS-2",
		},
		{
			name:     "Extension phase 5 string representation",
			phase:    fbc.LifecycleExtensionPhase(5),
			expected: "EUS-5",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			actual := test.phase.String()
			assert.Equal(t, test.expected, actual)
		})
	}
}
