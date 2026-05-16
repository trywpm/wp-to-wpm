package validate

func PackageName(name []byte) bool {
	n := len(name)
	if n < 3 || n > 164 {
		return false
	}

	for i := range n {
		c := name[i]

		// check for allowed characters a-z
		if c >= 'a' && c <= 'z' {
			continue
		}

		// check for allowed characters 0-9
		if c >= '0' && c <= '9' {
			continue
		}

		// check for allowed special characters `-`
		if c == '-' {
			if i == 0 || i == n-1 || name[i-1] == '-' {
				return false
			}
			continue
		}

		return false
	}

	return true
}
