package pairing

// wordlist: 256 short words (4-7 characters), easy to dictate, no obvious
// homophones between them. Used to build the 3-word pairing codes.
// With 3 independent draws: 256^3 ~= 16.7M combinations, more than enough
// when combined with PAKE (1 attempt per connection) and a code that
// expires after 10 minutes.
var wordlist = [256]string{
	"amber", "anchor", "apple", "april", "arrow", "atlas", "aspen", "autumn",
	"azure", "bacon", "badge", "baker", "august", "banjo", "barley", "basil",
	"basin", "beach", "beacon", "bean", "beetle", "belt", "berry", "bingo",
	"bishop", "bison", "black", "blaze", "blink", "block", "blossom", "blue",
	"bluff", "bolt", "bonus", "book", "boost", "border", "branch", "brass",
	"bravo", "bread", "breeze", "brick", "bridge", "bright", "broker", "bronze",
	"brook", "brown", "brush", "bubble", "buffalo", "bunny", "burger", "butter",
	"cactus", "cake", "camel", "canary", "candle", "canoe", "canyon", "carbon",
	"cargo", "carpet", "carrot", "castle", "cedar", "celery", "cello", "cement",
	"chalk", "charm", "cherry", "chess", "chest", "chime", "circus", "citrus",
	"clamp", "clarity", "cleric", "clever", "cliff", "clock", "cloud", "clover",
	"cobalt", "cocoa", "coffee", "comet", "copper", "coral", "cosmos", "cotton",
	"cousin", "coyote", "crane", "crater", "crayon", "creek", "cricket", "crimson",
	"crisp", "crystal", "cupid", "curry", "custard", "cyber", "cymbal", "dagger",
	"daisy", "dance", "danger", "dawn", "deer", "delta", "denim", "desert",
	"diamond", "diesel", "ditto", "dock", "domino", "donut", "dragon", "dream",
	"drift", "drum", "duke", "dust", "eagle", "earth", "echo", "ember",
	"emerald", "emoji", "energy", "engine", "epic", "evening", "fable", "falcon",
	"family", "fancy", "feather", "fern", "ferry", "fiber", "fiesta", "finger",
	"firefly", "fjord", "flame", "flask", "flint", "flower", "flute", "forest",
	"forge", "fortune", "fossil", "fox", "frame", "freckle", "fresh", "friday",
	"frost", "fudge", "fungus", "galaxy", "garden", "garlic", "gecko", "gemini",
	"ghost", "ginger", "glacier", "globe", "gloss", "golden", "gospel", "granite",
	"grape", "gravel", "green", "grizzly", "guava", "guitar", "gully", "gusto",
	"hammer", "harbor", "harvest", "hazel", "helmet", "hero", "hickory", "honey",
	"hornet", "hostel", "hotel", "hunter", "hurdle", "iceberg", "indigo", "inkpot",
	"iris", "island", "ivory", "jacket", "jaguar", "javelin", "jelly", "jewel",
	"jingle", "jolly", "jovial", "junior", "jungle", "kayak", "kettle", "kiwi",
	"knight", "lagoon", "lantern", "laser", "lattice", "lava", "lavender", "ledger",
	"lemon", "lentil", "lichen", "lilac", "linen", "lion", "liquid", "lobster",
	"lotus", "luna", "lyric", "magnet", "mango", "maple", "marble", "marine",
	"meadow", "melon", "memo", "meteor", "mineral", "mirror", "mistral", "monsoon",
}
