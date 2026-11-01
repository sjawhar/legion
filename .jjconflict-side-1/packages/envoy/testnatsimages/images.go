// Package testnatsimages is a public re-export of internal/testnats's NATS server images, so a
// module outside github.com/sjawhar/envoy (packages/daemon's real-server grant test) runs on the
// same images Envoy's tests run on: Go's internal-package rule keeps that module from importing
// internal/testnats itself. internal/testnats stays the one place the images are pinned.
package testnatsimages

import internal "github.com/sjawhar/envoy/internal/testnats"

// Image is internal/testnats.Image.
const Image = internal.Image

// HubImage is internal/testnats.HubImage, the image the GCP hub runs.
const HubImage = internal.HubImage

// Images is internal/testnats.Images: each image with the short name its subtest takes.
var Images = internal.Images
