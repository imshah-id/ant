package analyzer_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/skudra-go/ant/tools/antvet/analyzer"
)

func Test(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), analyzer.Analyzer, "models", "example", "dotted")
}
