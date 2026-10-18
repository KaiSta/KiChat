package llm

import "testing"

// TestOllamaSendsAGreedyZeroOnlyWhenItWasMeant pins the difference between "nobody chose
// a temperature" and "greedy".
//
// Ollama leaves the key out when the value is not above zero. That is right for a caller
// that said nothing — the model's own sampling applies — and wrong for a caller that asked
// for greedy, which is how BFL's caption recipe decodes: the modelfile's temperature takes
// over and two captions of one request stop matching.
func TestOllamaSendsAGreedyZeroOnlyWhenItWasMeant(t *testing.T) {
	if _, ok := ollamaOptions(ChatRequest{})["temperature"]; ok {
		t.Error("an unchosen temperature was sent; the backend's own default was overwritten")
	}
	greedy := ollamaOptions(ChatRequest{Temperature: 0, TemperatureSet: true})
	if v, ok := greedy["temperature"]; !ok || v != float64(0) {
		t.Errorf("temperature = %v (present = %v), want an explicit 0", v, ok)
	}
	if v, ok := ollamaOptions(ChatRequest{Temperature: 0.7})["temperature"]; !ok || v != 0.7 {
		t.Errorf("temperature = %v (present = %v), want 0.7", v, ok)
	}
}
