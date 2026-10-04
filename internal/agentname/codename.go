// Package agentname generates and disambiguates agent names.
package agentname

import "math/rand/v2"

var adjectives = []string{
	"amber", "ample", "arctic", "ashen", "autumn", "azure", "balmy", "bold", "brave", "breezy",
	"bright", "brisk", "calm", "candid", "clever", "cobalt", "cosmic", "crisp", "curious", "dapper",
	"dawn", "deft", "eager", "early", "earnest", "electric", "emerald", "fair", "fancy", "fleet",
	"fluffy", "frosty", "gentle", "gilded", "glad", "golden", "grand", "happy", "hardy", "hazel",
	"humble", "icy", "indigo", "ivory", "jade", "jolly", "keen", "kind", "lively", "lucid",
	"lucky", "lunar", "mellow", "merry", "mighty", "misty", "modest", "mossy", "nimble", "noble",
	"novel", "oaken", "olive", "opal", "patient", "peppy", "plucky", "polar", "proud", "quick",
	"quiet", "radiant", "rapid", "ready", "rosy", "royal", "ruby", "rustic", "sandy", "scarlet",
	"serene", "silent", "silver", "sleek", "snowy", "solar", "spry", "steady", "stellar", "sunny",
	"swift", "tidy", "topaz", "vivid", "warm", "wavy", "wild", "windy", "wise", "witty",
	"zesty", "zippy",
}

var nouns = []string{
	"alpaca", "badger", "bat", "bear", "beaver", "bison", "bobcat", "brook", "canyon", "caribou",
	"cedar", "cheetah", "cliff", "cloud", "condor", "cougar", "coyote", "crane", "creek", "deer",
	"dolphin", "dove", "dune", "eagle", "egret", "elk", "falcon", "fern", "finch", "fjord",
	"fox", "gazelle", "gecko", "glacier", "grove", "gull", "harbor", "hare", "hawk", "heron",
	"horizon", "ibis", "iguana", "island", "jaguar", "kestrel", "kiwi", "koala", "lagoon", "lark",
	"lemur", "leopard", "lion", "llama", "lynx", "maple", "marten", "meadow", "meerkat", "moose",
	"newt", "ocelot", "orca", "osprey", "otter", "owl", "panda", "panther", "pebble", "pelican",
	"pine", "plover", "pony", "puffin", "quail", "rabbit", "raven", "reef", "river", "robin",
	"salmon", "seal", "sparrow", "spruce", "squirrel", "stag", "stork", "stream", "summit", "swan",
	"tiger", "trout", "tundra", "turtle", "valley", "walrus", "whale", "willow", "wolf", "wren",
	"yak", "zebra",
}

// GenerateCodename returns a memorable adjective-noun slug such as
// "swift-falcon". The result always satisfies store.ValidateName.
func GenerateCodename() string {
	return adjectives[rand.IntN(len(adjectives))] + "-" + nouns[rand.IntN(len(nouns))]
}
