package skills

import "testing"

func TestProbeSexagesimal(t *testing.T) {
	for _, s := range []string{"1:30", "1:30.5", "1:30:00", "90.0", "1:30:00.5", "time: 1:30"} {
		v, err := yamlLoad(s)
		t.Logf("%-12q -> %#v err=%v", s, v, err)
	}
}
