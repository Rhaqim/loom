package engine

// Modality is the output kind a generator produces.
type Modality string

const (
	ModalityText  Modality = "text"
	ModalityImage Modality = "image"
	ModalityVideo Modality = "video"
	ModalityAudio Modality = "audio"
	// ModalityModel3D is a generated, renderable 3D asset (normally a GLB).
	// It is distinct from ModalityWorld, which carries logical world-state
	// deltas rather than a model file.
	ModalityModel3D    Modality = "model3d"
	ModalityWorld      Modality = "world"
	ModalityStructured Modality = "structured"
)
