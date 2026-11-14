package audio

import (
	"errors"
	"math"
)

// Resample は線形補間でサンプルレートを変える。レートが同じときはコピーを返す。
// 出力位置 i は入力の i * fromRate / toRate に対応する。端を超えた位置は最後のサンプルにする。
func Resample(in []int16, fromRate, toRate int) ([]int16, error) {
	if fromRate <= 0 || toRate <= 0 {
		return nil, errors.New("サンプルレートが不正です")
	}
	if len(in) == 0 || fromRate == toRate {
		out := make([]int16, len(in))
		copy(out, in)
		return out, nil
	}
	n := int(math.Round(float64(len(in)) * float64(toRate) / float64(fromRate)))
	if n < 1 {
		n = 1
	}
	out := make([]int16, n)
	if len(in) == 1 {
		for i := range out {
			out[i] = in[0]
		}
		return out, nil
	}
	last := float64(len(in) - 1)
	for i := range out {
		pos := float64(i) * float64(fromRate) / float64(toRate)
		if pos >= last {
			out[i] = in[len(in)-1]
			continue
		}
		index := int(pos)
		frac := pos - float64(index)
		value := float64(in[index])*(1-frac) + float64(in[index+1])*frac
		out[i] = clampInt16(value)
	}
	return out, nil
}

func clampInt16(value float64) int16 {
	if value > math.MaxInt16 {
		return math.MaxInt16
	}
	if value < math.MinInt16 {
		return math.MinInt16
	}
	return int16(math.Round(value))
}
