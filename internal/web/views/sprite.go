package views

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strings"
	"sync"

	lucide "github.com/kaugesaar/lucide-go"
)

// spriteIcons are the lucide icons the templates use. Icon renders these
// as a <use> of a symbol in the sprite, a fifth of the bytes of the
// inline SVG, which mattered: icons were a third of every page and of
// every sidebar redraw. The sprite sits at the top of every page's body
// (SpriteInline), so the references resolve on the first paint; as a
// separate file (/static/icons.svg) the icons showed up a beat after
// the text. A name outside the list still renders inline, so the list
// only needs to be complete for the saving, not for correctness;
// sprite_test.go checks the literal names in the templates.
var spriteIcons = []string{
	"archive", "archive-restore", "arrow-down", "arrow-left", "arrow-right", "arrow-up",
	"bot", "brain", "chart-no-axes-combined", "check", "chevron-down", "chevron-right", "chevrons-up",
	"circle", "circle-alert", "circle-arrow-up", "circle-check", "circle-dot", "circle-help", "circle-slash", "circle-x",
	"clipboard-paste", "columns-2", "command", "copy", "ellipsis-vertical", "external-link", "eye", "eye-off",
	"file", "folder", "folder-git-2", "folder-plus", "fold-vertical", "gauge",
	"git-branch", "git-compare-arrows", "git-merge", "git-pull-request", "git-pull-request-closed", "git-pull-request-create", "git-pull-request-draft",
	"fast-forward", "keyboard", "link", "list-end", "loader-circle", "log-in", "menu", "pin", "pin-off", "qr-code",
	"message-square", "message-square-plus", "message-square-x", "palette", "paperclip", "pencil", "pencil-line", "plug", "plus",
	"refresh-cw", "rotate-ccw", "rotate-cw", "save", "search", "settings", "shield-check", "square", "square-pen", "square-x", "star",
	"terminal", "trash-2", "triangle-alert", "undo-2", "unfold-horizontal", "unlink", "upload", "user", "x",
	"alarm-clock", "sun", "sunrise", "calendar", "calendar-clock", "history", "text-quote", "bookmark", "bookmark-plus",
	"zoom-in", "zoom-out", "chevrons-down-up", "chevrons-up-down", "list-filter", "file-text", "image", "maximize-2", "code",
}

// brandPaths are the brand marks as sprite symbols. The drivers' are
// drawn by brand.templ; the Claude sunburst alone is 1.8 KB of path, once
// per thread row. VS Code's is for the button that opens a thread in it,
// taken from Simple Icons 11, the last release that had it.
var brandPaths = map[string]string{
	"brand-claude": `m4.7144 15.9555 4.7174-2.6471.079-.2307-.079-.1275h-.2307l-.7893-.0486-2.6956-.0729-2.3375-.0971-2.2646-.1214-.5707-.1215-.5343-.7042.0546-.3522.4797-.3218.686.0608 1.5179.1032 2.2767.1578 1.6514.0972 2.4468.255h.3886l.0546-.1579-.1336-.0971-.1032-.0972L6.973 9.8356l-2.55-1.6879-1.3356-.9714-.7225-.4918-.3643-.4614-.1578-1.0078.6557-.7225.8803.0607.2246.0607.8925.686 1.9064 1.4754 2.4893 1.8336.3643.3035.1457-.1032.0182-.0728-.164-.2733-1.3539-2.4467-1.445-2.4893-.6435-1.032-.17-.6194c-.0607-.255-.1032-.4674-.1032-.7285L6.287.1335 6.6997 0l.9957.1336.419.3642.6192 1.4147 1.0018 2.2282 1.5543 3.0296.4553.8985.2429.8318.091.255h.1579v-.1457l.1275-1.706.2368-2.0947.2307-2.6957.0789-.7589.3764-.9107.7468-.4918.5828.2793.4797.686-.0668.4433-.2853 1.8517-.5586 2.9021-.3643 1.9429h.2125l.2429-.2429.9835-1.3053 1.6514-2.0643.7286-.8196.85-.9046.5464-.4311h1.0321l.759 1.1293-.34 1.1657-1.0625 1.3478-.8804 1.1414-1.2628 1.7-.7893 1.36.0729.1093.1882-.0183 2.8535-.607 1.5421-.2794 1.8396-.3157.8318.3886.091.3946-.3278.8075-1.967.4857-2.3072.4614-3.4364.8136-.0425.0304.0486.0607 1.5482.1457.6618.0364h1.621l3.0175.2247.7892.522.4736.6376-.079.4857-1.2142.6193-1.6393-.3886-3.825-.9107-1.3113-.3279h-.1822v.1093l1.0929 1.0686 2.0035 1.8092 2.5075 2.3314.1275.5768-.3218.4554-.34-.0486-2.2039-1.6575-.85-.7468-1.9246-1.621h-.1275v.17l.4432.6496 2.3436 3.5214.1214 1.0807-.17.3521-.6071.2125-.6679-.1214-1.3721-1.9246L14.38 17.959l-1.1414-1.9428-.1397.079-.674 7.2552-.3156.3703-.7286.2793-.6071-.4614-.3218-.7468.3218-1.4753.3886-1.9246.3157-1.53.2853-1.9004.17-.6314-.0121-.0425-.1397.0182-1.4328 1.9672-2.1796 2.9446-1.7243 1.8456-.4128.164-.7164-.3704.0667-.6618.4008-.5889 2.386-3.0357 1.4389-1.882.929-1.0868-.0062-.1579h-.0546l-6.3385 4.1164-1.1293.1457-.4857-.4554.0608-.7467.2307-.2429 1.9064-1.3114Z`,
	"brand-codex":  `M22.2819 9.8211a5.9847 5.9847 0 0 0-.5157-4.9108 6.0462 6.0462 0 0 0-6.5098-2.9A6.0651 6.0651 0 0 0 4.9807 4.1818a5.9847 5.9847 0 0 0-3.9977 2.9 6.0462 6.0462 0 0 0 .7427 7.0966 5.98 5.98 0 0 0 .511 4.9107 6.051 6.051 0 0 0 6.5146 2.9001A5.9847 5.9847 0 0 0 13.2599 24a6.0557 6.0557 0 0 0 5.7718-4.2058 5.9894 5.9894 0 0 0 3.9977-2.9001 6.0557 6.0557 0 0 0-.7475-7.0729zm-9.022 12.6081a4.4755 4.4755 0 0 1-2.8764-1.0408l.1419-.0804 4.7783-2.7582a.7948.7948 0 0 0 .3927-.6813v-6.7369l2.02 1.1686a.071.071 0 0 1 .038.052v5.5826a4.504 4.504 0 0 1-4.4945 4.4944zm-9.6607-4.1254a4.4708 4.4708 0 0 1-.5346-3.0137l.142.0852 4.783 2.7582a.7712.7712 0 0 0 .7806 0l5.8428-3.3685v2.3324a.0804.0804 0 0 1-.0332.0615L9.74 19.9502a4.4992 4.4992 0 0 1-6.1408-1.6464zM2.3408 7.8956a4.485 4.485 0 0 1 2.3655-1.9728V11.6a.7664.7664 0 0 0 .3879.6765l5.8144 3.3543-2.0201 1.1685a.0757.0757 0 0 1-.071 0l-4.8303-2.7865A4.504 4.504 0 0 1 2.3408 7.872zm16.5963 3.8558L13.1038 8.364 15.1192 7.2a.0757.0757 0 0 1 .071 0l4.8303 2.7913a4.4944 4.4944 0 0 1-.6765 8.1042v-5.6772a.79.79 0 0 0-.407-.667zm2.0107-3.0231l-.142-.0852-4.7735-2.7818a.7759.7759 0 0 0-.7854 0L9.409 9.2297V6.8974a.0662.0662 0 0 1 .0284-.0615l4.8303-2.7866a4.4992 4.4992 0 0 1 6.6802 4.66zM8.3065 12.863l-2.02-1.1638a.0804.0804 0 0 1-.038-.0567V6.0742a4.4992 4.4992 0 0 1 7.3757-3.4537l-.142.0805L8.704 5.459a.7948.7948 0 0 0-.3927.6813zm1.0976-2.3654l2.602-1.4998 2.6069 1.4998v2.9994l-2.5974 1.4997-2.6067-1.4997Z`,
	"brand-vscode": `M23.15 2.587L18.21.21a1.494 1.494 0 0 0-1.705.29l-9.46 8.63-4.12-3.128a.999.999 0 0 0-1.276.057L.327 7.261A1 1 0 0 0 .326 8.74L3.899 12 .326 15.26a1 1 0 0 0 .001 1.479L1.65 17.94a.999.999 0 0 0 1.276.057l4.12-3.128 9.46 8.63a1.492 1.492 0 0 0 1.704.29l4.942-2.377A1.5 1.5 0 0 0 24 20.06V3.939a1.5 1.5 0 0 0-.85-1.352zm-5.146 14.861L10.826 12l7.178-5.448v10.896z`,
}

var (
	spriteSet     map[string]bool
	spriteOnce    sync.Once
	spriteBody    string
	spriteVersion string
	svgInner      = regexp.MustCompile(`(?s)<svg[^>]*>(.*)</svg>`)
)

func init() {
	spriteSet = map[string]bool{}
	for _, n := range spriteIcons {
		spriteSet[n] = true
	}
}

// buildSprite renders every listed icon once and keeps the paths as
// symbols. The version is a hash of the result, so the sprite URL changes
// when the list or lucide does and browsers may cache it for good.
func buildSprite() {
	names := append([]string(nil), spriteIcons...)
	sort.Strings(names)
	var b strings.Builder
	b.WriteString(`<svg xmlns="http://www.w3.org/2000/svg">`)
	for _, n := range names {
		m := svgInner.FindStringSubmatch(string(lucide.Icon(n)))
		if m == nil {
			continue
		}
		b.WriteString(`<symbol id="` + n + `" viewBox="0 0 24 24">` + m[1] + `</symbol>`)
	}
	for _, n := range []string{"brand-claude", "brand-codex", "brand-vscode"} {
		b.WriteString(`<symbol id="` + n + `" viewBox="0 0 24 24"><path d="` + brandPaths[n] + `"/></symbol>`)
	}
	b.WriteString(`</svg>`)
	spriteBody = b.String()
	sum := sha256.Sum256([]byte(spriteBody))
	spriteVersion = hex.EncodeToString(sum[:6])
}

// Sprite is the icon sheet the web package serves at /static/icons.svg.
func Sprite() (body, version string) {
	spriteOnce.Do(buildSprite)
	return spriteBody, spriteVersion
}

// spriteRef is the <use> target of a listed icon: a symbol of the
// sprite inlined in the page.
func spriteRef(name string) string {
	return "#" + name
}

// SpriteInline is the sprite as a hidden element for the top of the body.
// Zero size rather than display: none, which some engines take as "do
// not render the symbols either".
func SpriteInline() string {
	body, _ := Sprite()
	return strings.Replace(body, `<svg xmlns="http://www.w3.org/2000/svg">`, `<svg xmlns="http://www.w3.org/2000/svg" style="position: absolute; width: 0; height: 0; overflow: hidden" aria-hidden="true">`, 1)
}
