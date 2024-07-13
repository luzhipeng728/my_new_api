package main

import (
	"fmt"
	"math"
	"strings"
)

// 9kTbB 1720863893

const (
	dictionary = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	idLength   = 5
)

func GenerateID(timestamp int64) string {
	timestamp -= 1567879599
	fmt.Println(timestamp)
	var result strings.Builder
	for i := idLength - 1; i >= 0; i-- {
		index := int(math.Mod(float64(timestamp), float64(len(dictionary))))
		fmt.Println("index_1: ", index)
		result.WriteByte(dictionary[index-1])
		timestamp = timestamp / int64(len(dictionary))
	}
	// Reverse the string
	runes := []rune(result.String())
	for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
		runes[i], runes[j] = runes[j], runes[i]
	}
	return string(runes)
}

func tmp(tmp string, t int64) {
	tmp_10 := int64(0)
	for i := 4; i >= 0; i-- {
		fmt.Println(string(tmp[i]))
		index := int64(strings.Index(dictionary, string(tmp[i]))) + 1
		fmt.Println("index: ", index)
		tmp_10 += int64(math.Pow(float64(len(dictionary)), float64(4-i))) * int64(index)
		// fmt.Println(tmp_10)
	}
	fmt.Println(tmp_10)
	diff := t - tmp_10
	fmt.Println(diff)
}

func main() {
	tmp("9kTbB", int64(1720863893))
	generateID := GenerateID(1720863893)
	fmt.Println(generateID)
	tmp("9kTbV", int64(1720863913))
}
