package retrieve

import "sort"

func sortResultsDesc(r []Result) {
	sort.Slice(r, func(i, j int) bool { return r[i].Score > r[j].Score })
}

func sortKVDesc(kv []idScore) {
	sort.Slice(kv, func(i, j int) bool { return kv[i].score > kv[j].score })
}
