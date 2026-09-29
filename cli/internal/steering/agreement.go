package steering

// Pair is one intervention labelled by a person (Gold) and by the model. Classified is false
// when the model's answer was invalid, failed or below the threshold.
type Pair struct {
	Gold       Label
	Model      Label
	Classified bool
}

// Score is the agreement of one step: the share of equal labels and Cohen's kappa, over N pairs.
type Score struct {
	N         int
	Agreement float64
	Kappa     float64
}

// Agreement compares the model with people on a set of interventions.
type Agreement struct {
	Total        int
	Unclassified int
	Intent       Score // over every classified pair
	WentWrong    Score // over classified pairs both sides call a correction
	Prevention   Score // the same pairs
	Confusion    map[string]map[string]int
}

// UnclassifiedShare is the share of pairs the model left unclassified.
func (a Agreement) UnclassifiedShare() float64 {
	if a.Total == 0 {
		return 0
	}
	return float64(a.Unclassified) / float64(a.Total)
}

// Agree scores the pairs step by step. The confusion table counts intents: person, then model.
func Agree(pairs []Pair) Agreement {
	a := Agreement{Total: len(pairs), Confusion: map[string]map[string]int{}}
	var goldIntent, modelIntent, goldWrong, modelWrong, goldPrev, modelPrev []string
	for _, p := range pairs {
		if !p.Classified {
			a.Unclassified++
			continue
		}
		goldIntent = append(goldIntent, p.Gold.Intent)
		modelIntent = append(modelIntent, p.Model.Intent)
		if a.Confusion[p.Gold.Intent] == nil {
			a.Confusion[p.Gold.Intent] = map[string]int{}
		}
		a.Confusion[p.Gold.Intent][p.Model.Intent]++
		if p.Gold.Intent == "correction" && p.Model.Intent == "correction" {
			goldWrong = append(goldWrong, p.Gold.WentWrong)
			modelWrong = append(modelWrong, p.Model.WentWrong)
			goldPrev = append(goldPrev, p.Gold.Prevention)
			modelPrev = append(modelPrev, p.Model.Prevention)
		}
	}
	a.Intent = Kappa(goldIntent, modelIntent)
	a.WentWrong = Kappa(goldWrong, modelWrong)
	a.Prevention = Kappa(goldPrev, modelPrev)
	return a
}

// Kappa returns the observed agreement of two raters and Cohen's kappa,
// (po - pe) / (1 - pe), where pe is the agreement expected by chance from each rater's own label
// frequencies. When both raters use one and the same label throughout, pe is 1 and kappa is 1.
func Kappa(a, b []string) Score {
	n := min(len(a), len(b))
	if n == 0 {
		return Score{}
	}
	agree := 0
	countA, countB := map[string]int{}, map[string]int{}
	for i := range n {
		if a[i] == b[i] {
			agree++
		}
		countA[a[i]]++
		countB[b[i]]++
	}
	po := float64(agree) / float64(n)
	pe := 0.0
	for label, ca := range countA {
		pe += float64(ca) / float64(n) * float64(countB[label]) / float64(n)
	}
	if pe >= 1 {
		return Score{N: n, Agreement: po, Kappa: 1}
	}
	return Score{N: n, Agreement: po, Kappa: (po - pe) / (1 - pe)}
}
