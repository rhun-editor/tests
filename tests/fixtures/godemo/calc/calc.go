package calc

func Add(a, b int) int {
	if a < 0 {
		return -1
	}
	return a + b
}

func Greet(name string) string {
	return "Hello, " + name
}
