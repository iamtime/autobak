package main

import (
	"flag"
	"reflect"
	"testing"
)

func TestHoistFlags(t *testing.T) {
	cases := []struct {
		in, want []string
	}{
		// имя сервера перед флагом - главный случай из бага
		{[]string{"prod", "--binary", "agent"}, []string{"--binary", "agent", "prod"}},
		// флаг уже первый - ничего не меняем
		{[]string{"--binary", "agent", "prod"}, []string{"--binary", "agent", "prod"}},
		// только позиционные - без изменений
		{[]string{"prod", "snap123"}, []string{"prod", "snap123"}},
		// два позиционных перед флагами (restore), порядок сохраняется
		{[]string{"prod", "snap", "--to-server", "--apply"},
			[]string{"--to-server", "--apply", "prod", "snap"}},
		{nil, nil},
	}
	for _, c := range cases {
		if got := hoistFlags(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("hoistFlags(%v) = %v, ожидалось %v", c.in, got, c.want)
		}
	}
}

// Сквозная проверка: флаг после позиционного действительно парсится.
func TestHoistFlagsParses(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	binary := fs.String("binary", "", "")
	if err := fs.Parse(hoistFlags([]string{"prod", "--binary", "agent"})); err != nil {
		t.Fatal(err)
	}
	if *binary != "agent" {
		t.Fatalf("--binary не разобран: %q", *binary)
	}
	if fs.Arg(0) != "prod" {
		t.Fatalf("позиционный аргумент потерян: %q", fs.Arg(0))
	}
}
