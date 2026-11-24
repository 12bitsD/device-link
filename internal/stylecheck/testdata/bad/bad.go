package bad

import (
	"context"
	"errors"
	"log"
	"os"
)

var globalCounter int // want "BCS011"

var ErrSentinel = errors.New("sentinel")

type inner struct{}

type badEmbed struct {
	inner // want "GO010"
}

type badContext struct {
	ctx context.Context // want "GO003"
}

func init() { // want "GO008"
	globalCounter = 1
}

// TODO: missing owner and condition // want "BCS015"
func badParams(ctx context.Context, other context.Context, flag bool, a, b, c, d int) { // want "GO003" "BCS004"
	flag = true // want "BCS011"
	if flag {
		flag := false // want "BCS011"
		_ = flag
	}
	_ = other
	_ = a + b + c + d
}

func redundantElse(x int) int {
	if x > 0 {
		return x
	} else { // want "BCS005"
		return 0
	}
}

func localShadow() {
	count := 1
	{
		count := 2 // want "BCS011"
		_ = count
	}
}

// bits:allow(BCS004): fixture isolates nesting and panic checks from bool params
func deepNesting(a, b, c, d bool) { // want "BCS003"
	if a {
		if b {
			if c {
				if d {
					panic("boom") // want "GO001"
				}
			}
		}
	}
}

func exitsAndLogs() {
	if globalCounter > 10 {
		os.Exit(1) // want "GO001"
	}
	if globalCounter > 5 {
		log.Fatal("fatal") // want "GO001"
	}
	if globalCounter > 2 { // want "GO001"
	}
}
