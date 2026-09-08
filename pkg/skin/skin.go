// Package skin gives the agent a visible appearance.
//
// Bedrock skins are uploaded by the client, not fetched from the account: your
// real skin lives in your Minecraft installation and is sent at login. A
// headless client has no installation and therefore no skin, so gophertunnel
// substitutes a placeholder -- a solid black 64x32 buffer, under a SkinID
// regenerated on every connect (minecraft/dial.go). That is why these accounts
// appear as featureless silhouettes that flicker identity between sessions.
//
// The skin here is generated rather than shipped as an asset: a colour derived
// from the account name means every bot on the server is a different colour
// without anyone choosing one, which is also the cheapest fix for a problem
// that cost real time tonight -- working out which Deployment was which player
// required correlating pod timestamps against connection logs, because nothing
// identified them.
package skin

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// Bedrock's modern skin format. The dialer's own default is the legacy 64x32,
// which renders without the second layer and the separate limbs.
const (
	Width  = 64
	Height = 64
)

// Skin is everything the login handshake needs to describe an appearance.
// Geometry and the resource patch are deliberately absent: gophertunnel
// supplies working defaults for both, and overriding them means shipping a
// geometry JSON whose only purpose would be to say "humanoid".
type Skin struct {
	// ID is stable for a given name. The dialer otherwise generates a fresh
	// UUID per connection, so a reconnecting bot looks like a different skin
	// to clients that cache by ID.
	ID string
	// Data is base64-encoded raw RGBA, row-major, Width*Height*4 bytes.
	Data   string
	Width  int
	Height int
}

// For builds a deterministic skin for an account name.
//
// Same name in, same skin out -- across restarts, across pods, across
// rebuilds. A skin that changed on redeploy would be worse than no skin: it
// would look like a different player each time the agent rolled.
func For(name string) Skin {
	sum := sha256.Sum256([]byte(name))
	base := colourFrom(sum)
	// A darker shade for the torso, so the figure reads as clothed rather than
	// as a single flat block of colour at distance.
	shirt := darken(base, 0.65)
	// Near-black, not pure: pure black is what an unset skin looks like, and
	// the eyes should not be the one part that resembles the bug.
	eye := rgba{20, 20, 28, 255}

	px := make([]byte, Width*Height*4)
	for i := 0; i < Width*Height; i++ {
		copy(px[i*4:], base.bytes())
	}
	// Torso front, in the standard 64x64 UV layout.
	fill(px, 20, 20, 8, 12, shirt)
	// Two eyes on the head's front face, which occupies x 8-15, y 8-15.
	fill(px, 10, 11, 2, 1, eye)
	fill(px, 13, 11, 2, 1, eye)

	return Skin{
		ID:     fmt.Sprintf("%x-agent", sum[:8]),
		Data:   base64.StdEncoding.EncodeToString(px),
		Width:  Width,
		Height: Height,
	}
}

type rgba struct{ r, g, b, a byte }

func (c rgba) bytes() []byte { return []byte{c.r, c.g, c.b, c.a} }

// colourFrom picks a saturated, mid-brightness colour from the hash.
//
// Generated through hue rather than by taking three bytes as RGB: random RGB
// lands on muddy browns and near-blacks often enough that two bots could end
// up indistinguishable, which defeats the purpose.
func colourFrom(sum [32]byte) rgba {
	hue := float64(uint16(sum[0])<<8|uint16(sum[1])) / 65535 * 360
	return hsv(hue, 0.65, 0.85)
}

func darken(c rgba, factor float64) rgba {
	return rgba{byte(float64(c.r) * factor), byte(float64(c.g) * factor), byte(float64(c.b) * factor), c.a}
}

func hsv(h, s, v float64) rgba {
	c := v * s
	x := c * (1 - abs(mod(h/60, 2)-1))
	m := v - c
	var r, g, b float64
	switch {
	case h < 60:
		r, g, b = c, x, 0
	case h < 120:
		r, g, b = x, c, 0
	case h < 180:
		r, g, b = 0, c, x
	case h < 240:
		r, g, b = 0, x, c
	case h < 300:
		r, g, b = x, 0, c
	default:
		r, g, b = c, 0, x
	}
	return rgba{byte((r + m) * 255), byte((g + m) * 255), byte((b + m) * 255), 255}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

func mod(a, b float64) float64 {
	for a >= b {
		a -= b
	}
	return a
}

// fill paints a rectangle, clipped to the skin bounds so a bad rectangle
// cannot corrupt neighbouring pixels or panic on a slice bound.
func fill(px []byte, x, y, w, h int, c rgba) {
	for dy := 0; dy < h; dy++ {
		for dx := 0; dx < w; dx++ {
			px0, py := x+dx, y+dy
			if px0 < 0 || px0 >= Width || py < 0 || py >= Height {
				continue
			}
			copy(px[(py*Width+px0)*4:], c.bytes())
		}
	}
}
