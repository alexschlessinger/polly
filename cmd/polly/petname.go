package main

import (
	"fmt"
	"math/rand/v2"
)

// Word lists for generated context names. Short, unambiguous, and
// lowercase-only so the names are easy to retype with -c.
var (
	petAdjectives = []string{
		"agile", "amber", "bold", "brave", "bright", "brisk", "calm",
		"clever", "cosmic", "crisp", "curious", "daring", "deft", "dusty",
		"eager", "fleet", "fond", "gentle", "glad", "golden", "happy",
		"hardy", "humble", "jolly", "keen", "kind", "lively", "lucid",
		"lunar", "mellow", "merry", "misty", "nimble", "noble", "patient",
		"placid", "plucky", "proud", "quick", "quiet", "rustic", "sly",
		"snug", "spry", "stout", "sunny", "swift", "tidy", "vivid", "witty",
	}
	petAnimals = []string{
		"badger", "bat", "bear", "beaver", "bison", "crane", "crow",
		"deer", "dove", "egret", "falcon", "ferret", "finch", "fox",
		"gecko", "hare", "heron", "ibex", "koala", "lemur", "llama",
		"lynx", "marmot", "marten", "mole", "moose", "newt", "otter",
		"owl", "panda", "pika", "quail", "rabbit", "raven", "robin",
		"seal", "shrew", "sparrow", "stoat", "swan", "tapir", "tern",
		"toad", "trout", "vole", "walrus", "weasel", "wolf", "wombat",
		"wren",
	}
)

// generateContextName returns a random adjective-animal context name that
// does not collide with an existing context, falling back to a numbered
// suffix if the namespace is crowded. An error from exists ends the search
// at once: treating it as "taken" would loop forever on a store that keeps
// failing (a canceled context, a closed store).
func generateContextName(exists func(string) (bool, error)) (string, error) {
	pick := func() string {
		return petAdjectives[rand.IntN(len(petAdjectives))] + "-" + petAnimals[rand.IntN(len(petAnimals))]
	}
	for range 32 {
		name := pick()
		taken, err := exists(name)
		if err != nil {
			return "", err
		}
		if !taken {
			return name, nil
		}
	}
	base := pick()
	for i := 2; ; i++ {
		name := fmt.Sprintf("%s-%d", base, i)
		taken, err := exists(name)
		if err != nil {
			return "", err
		}
		if !taken {
			return name, nil
		}
	}
}
