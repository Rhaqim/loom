// Package game contains D&D gameplay logic that sits on top of loom.
package game

import (
	"fmt"
	"math/rand/v2"
)

// Roll rolls n dice each with the given number of sides and returns the sum.
// e.g., Roll(2, 6) rolls 2d6.
func Roll(n, sides int) int {
	total := 0
	for range n {
		total += rand.IntN(sides) + 1
	}
	return total
}

// Standard dice convenience functions.
func D4() int   { return Roll(1, 4) }
func D6() int   { return Roll(1, 6) }
func D8() int   { return Roll(1, 8) }
func D10() int  { return Roll(1, 10) }
func D12() int  { return Roll(1, 12) }
func D20() int  { return Roll(1, 20) }
func D100() int { return Roll(1, 100) }

// RollExpr evaluates a simple dice expression like "2d6", "1d20", "3d4".
// Returns (result, description, error).
func RollExpr(expr string) (int, string, error) {
	var n, sides int
	if _, err := fmt.Sscanf(expr, "%dd%d", &n, &sides); err != nil {
		return 0, "", fmt.Errorf("invalid dice expression %q (use NdS format, e.g. 2d6)", expr)
	}
	if n < 1 || sides < 2 {
		return 0, "", fmt.Errorf("invalid dice: %dd%d", n, sides)
	}
	result := Roll(n, sides)
	return result, fmt.Sprintf("[%s = %d]", expr, result), nil
}

// AbilityCheck rolls a d20 and adds a modifier. Returns total and a description.
// DC is only used for the formatted description.
func AbilityCheck(modifier, dc int) (total int, success bool, desc string) {
	roll := D20()
	total = roll + modifier
	success = total >= dc
	outcome := "FAIL"
	if success {
		outcome = "PASS"
	}
	desc = fmt.Sprintf("[d20(%d) + %+d = %d vs DC %d → %s]", roll, modifier, total, dc, outcome)
	return
}

// AttackRoll performs a d20 attack roll. Returns (roll, total, isCrit, isFumble).
func AttackRoll(abilityMod, profBonus int) (roll, total int, isCrit, isFumble bool) {
	roll = D20()
	isCrit = roll == 20
	isFumble = roll == 1
	total = roll + abilityMod + profBonus
	return
}

// RollStats generates a set of 6 ability scores using the 4d6-drop-lowest method.
func RollStats() [6]int {
	var scores [6]int
	for i := range 6 {
		rolls := [4]int{D6(), D6(), D6(), D6()}
		// find and drop the lowest
		minIdx := 0
		for j := 1; j < 4; j++ {
			if rolls[j] < rolls[minIdx] {
				minIdx = j
			}
		}
		sum := 0
		for j, r := range rolls {
			if j != minIdx {
				sum += r
			}
		}
		scores[i] = sum
	}
	return scores
}

// BaseHPForClass returns the starting HP for a class at level 1.
func BaseHPForClass(class string, conMod int) int {
	hitDie := hitDieForClass(class)
	return hitDie + conMod
}

func hitDieForClass(class string) int {
	switch class {
	case "Barbarian":
		return 12
	case "Fighter", "Paladin", "Ranger":
		return 10
	case "Bard", "Cleric", "Druid", "Monk", "Rogue", "Warlock":
		return 8
	default: // Sorcerer, Wizard, etc.
		return 6
	}
}

// StartingACForClass returns a reasonable default AC for a class.
func StartingACForClass(class string, dexMod int) int {
	switch class {
	case "Barbarian":
		return 10 + dexMod // unarmored defense uses CON too but simplified
	case "Fighter", "Paladin":
		return 16 // assume chain mail
	case "Rogue", "Ranger", "Bard":
		return 13 + dexMod // leather armor
	default:
		return 10 + dexMod
	}
}
