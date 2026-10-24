package harnesseval

import (
	"bytes"
	"os"
	"testing"
)

func TestPrivateExpressionsCoverCatalog(t *testing.T) {
	fixtures, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fixtures {
		t.Run(f.Task.ID, func(t *testing.T) {
			cases, err := behaviorCases(f.PrivateTests)
			if err != nil || len(cases) == 0 {
				t.Fatalf("private case contract: %v", err)
			}
		})
	}
}

func TestCandidateDoesNotReceiveExpectedAnswers(t *testing.T) {
	const secret = "EXPECTED_ANSWER_MUST_STAY_IN_TRUSTED_GRADER_7f9a"
	cases, err := behaviorCases("package fixture\nimport(\"testing\";\"fmt\")\nfunc TestAcceptance(t *testing.T){if got:=fmt.Sprint(Probe());got!=" + `"` + secret + `"` + "{t.Fatal(got)}}")
	if err != nil {
		t.Fatal(err)
	}
	source := Source{"main.go": "package fixture\nimport(\"os\";\"strings\")\nfunc Probe()string{entries,_:=os.ReadDir(\".\");names:=[]string{};for _,e:=range entries{names=append(names,e.Name())};return strings.Join(names,\",\")}\n"}
	program, build, err := buildProgram(t.Context(), source, []string{cases[0].Expression})
	if err != nil || program == nil {
		t.Fatalf("build: %v %s", err, build.Stderr)
	}
	defer program.Close()
	data, err := os.ReadFile(program.binary)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(secret)) {
		t.Fatal("expected answer was compiled into candidate")
	}
	answer, completed, run := program.query(t.Context(), 0)
	if !completed || answer != "program" {
		t.Fatalf("candidate runtime contained grading sources: %q %v %+v", answer, completed, run)
	}
}
