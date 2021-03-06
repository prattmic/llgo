package main

import (
	"testing"
)

func TestCircularType(t *testing.T) {
	err := runAndCheckMain(testdata("circulartype.go"), checkStringsEqual)
	if err != nil {
		t.Fatal(err)
	}
}

func TestEmbeddedStruct(t *testing.T) {
	err := runAndCheckMain(testdata("structs/embed.go"), checkStringsEqual)
	if err != nil {
		t.Fatal(err)
	}
}

// vim: set ft=go:
