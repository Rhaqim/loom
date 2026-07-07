package loom

// Modality is the output kind a generator produces.
type Modality string

const (
	ModalityText       Modality = "text"
	ModalityImage      Modality = "image"
	ModalityVideo      Modality = "video"
	ModalityAudio      Modality = "audio"
	ModalityWorld      Modality = "world"
	ModalityStructured Modality = "structured"
)
