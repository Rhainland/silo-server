# Artwork storage

In **Settings → Infrastructure → Artwork storage**, choose Automatic, Local
disk, or S3. Automatic uses the public S3 bucket when one is configured and local
disk otherwise. Changes require a server restart.

Local disk suits a single server. Persist the displayed artwork directory in
Docker; it contains provider caches and uploads. S3 is recommended when multiple
hosts share a catalog, so every host can read the same artwork.

Provider artwork caching is enabled by default on new installations and works
with either backend. The setup wizard can finish without configuring S3.

## Changing storage after files are stored

The first artwork write records where artwork lives. From then on the backend,
the local artwork path, and, for S3, the public endpoint, bucket, and key prefix
no longer save directly. The first write to a private bucket records it the same
way, and its endpoint, bucket, and key prefix lock too. The settings page opens a
managed storage transition for these changes instead.

A transition verifies the new location, copies what the chosen policy covers,
switches the settings, and asks for a restart:

- **Start fresh** reads nothing from the old storage. Provider artwork is
  downloaded again from its saved source; uploads, downloaded subtitles, and
  avatars do not move.
- **Preserve personal uploads** copies branding, collection and library posters,
  and downloaded subtitles, plus profile avatars when their location changes.
  Provider artwork is downloaded again.
- **Migrate everything** copies the whole artwork tree, subtitles, avatars,
  diagnostic bundles, and catalog export files.

Switching from S3 to local disk moves everything to local disk, including what
the private bucket held. A local install can add or remove a private bucket the
same way. Silo never deletes the old storage; remove it yourself once the
transition and its restart have finished.

## Profile avatars and private files

Profile avatars, diagnostic bundles, and catalog export files use the private S3
bucket when one is configured. Otherwise a local install keeps them on local
disk. The public artwork bucket never receives them, so an S3 install needs a
private bucket for avatar uploads.
