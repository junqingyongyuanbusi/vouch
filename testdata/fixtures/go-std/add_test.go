package gostd
import "testing"
func TestAdd(t *testing.T){if Add(1,2)!=3{t.Fatal("fail")}}
func TestRegression(t *testing.T){if Add(1,2)!=3{t.Fatal("regression")}}
