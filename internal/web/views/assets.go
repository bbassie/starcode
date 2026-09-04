package views

// assetVersion tags every /static URL. The web package sets it to a hash of
// the embedded files at startup, so a rebuild changes the URLs and a normal
// refresh picks up new assets instead of needing a hard one.
var assetVersion = "dev"

func SetAssetVersion(v string) {
	if v != "" {
		assetVersion = v
	}
}

func asset(path string) string {
	return path + "?v=" + assetVersion
}
