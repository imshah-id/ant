package postgres

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPointScan(t *testing.T) {
	t.Run("nil source resets point", func(t *testing.T) {
		pt := Point{X: 10, Y: 20}
		require.NoError(t, pt.Scan(nil))
		require.Equal(t, Point{}, pt)
	})

	t.Run("standard float coordinates", func(t *testing.T) {
		var pt Point
		require.NoError(t, pt.Scan("(1.5,2.5)"))
		require.Equal(t, Point{X: 1.5, Y: 2.5}, pt)

		require.NoError(t, pt.Scan([]byte("(-3.25, 4.75)")))
		require.Equal(t, Point{X: -3.25, Y: 4.75}, pt)
	})

	t.Run("infinity coordinates from postgres (#52)", func(t *testing.T) {
		var pt Point
		require.NoError(t, pt.Scan("(Infinity,-Infinity)"))
		require.True(t, math.IsInf(pt.X, 1))
		require.True(t, math.IsInf(pt.Y, -1))

		require.NoError(t, pt.Scan("(+Infinity,Infinity)"))
		require.True(t, math.IsInf(pt.X, 1))
		require.True(t, math.IsInf(pt.Y, 1))

		valPosInf, err := (Point{X: math.Inf(1), Y: math.Inf(-1)}).Value()
		require.NoError(t, err)
		require.NoError(t, pt.Scan(valPosInf))
		require.True(t, math.IsInf(pt.X, 1))
		require.True(t, math.IsInf(pt.Y, -1))
	})

	t.Run("nan coordinates", func(t *testing.T) {
		var pt Point
		require.NoError(t, pt.Scan("(NaN,NaN)"))
		require.True(t, math.IsNaN(pt.X))
		require.True(t, math.IsNaN(pt.Y))
	})

	t.Run("value round-trip", func(t *testing.T) {
		pt := Point{X: 12.34, Y: 56.78}
		val, err := pt.Value()
		require.NoError(t, err)
		require.Equal(t, "(12.34,56.78)", val)

		var scanned Point
		require.NoError(t, scanned.Scan(val))
		require.Equal(t, pt, scanned)
	})

	t.Run("invalid formats return error", func(t *testing.T) {
		var pt Point
		require.Error(t, pt.Scan(12345))
		require.Error(t, pt.Scan("invalid-text"))
		require.Error(t, pt.Scan("10.5, 20.5"))
		require.Error(t, pt.Scan("(1,2) extra"))
		require.Error(t, pt.Scan("(1,2,3)"))
		require.Error(t, pt.Scan("(foo,bar)"))
	})
}
