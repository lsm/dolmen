package value

type nulFrame struct {
	object    bool
	expectKey bool
	key       string
}

func FindJSONNUL(doc []byte) (field string, inName bool, found bool) {
	var stack []nulFrame
	for i := 0; i < len(doc); i++ {
		switch doc[i] {
		case '{':
			stack = append(stack, nulFrame{object: true, expectKey: true})
		case '[':
			stack = append(stack, nulFrame{})
		case '}', ']':
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		case ',':
			if n := len(stack); n > 0 && stack[n-1].object {
				stack[n-1].expectKey = true
			}
		case '"':
			start := i + 1
			hasNUL := false
			j := start
			for ; j < len(doc) && doc[j] != '"'; j++ {
				if doc[j] == 0 {
					hasNUL = true
				}
				if doc[j] == '\\' && j+1 < len(doc) {
					if doc[j+1] == 'u' && j+5 < len(doc) && string(doc[j+2:j+6]) == "0000" {
						hasNUL = true
					}
					j++
				}
			}
			end := min(j, len(doc))
			i = j
			n := len(stack)
			if n > 0 && stack[n-1].object && stack[n-1].expectKey {
				stack[n-1].expectKey = false
				stack[n-1].key = string(doc[start:end])
				if hasNUL {
					return "", true, true
				}
				continue
			}
			if hasNUL {
				for k := n - 1; k >= 0; k-- {
					if stack[k].key != "" {
						return stack[k].key, false, true
					}
				}
				return "", false, true
			}
		}
	}
	return "", false, false
}
