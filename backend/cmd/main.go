package main

import (
	"log"
	"os"

	"llama-lab/backend/internal/app"
)

func main() {
	if err := app.Run(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}
