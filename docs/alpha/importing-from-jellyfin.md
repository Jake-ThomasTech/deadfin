# Import from Jellyfin

Deadfin reads a **copy** of Jellyfin's database and makes its own records. It
never writes to Jellyfin's files. Your Jellyfin server stays as it was, so you
can go back at any time. Changes that you make in Deadfin do not go back to
Jellyfin.

## What the import brings across

| Brought across | Not brought across |
|---|---|
| Users, with their passwords and policies (library access, parental rating, permissions) | Plugins and plugin settings (Deadfin has no plugins, D-08) |
| Device sign-ins (clients stay signed in) and API keys | Live TV and SyncPlay (not in Deadfin, D-07) |
| Watch state: played, play count, resume position, favorites, ratings, last audio and subtitle choice | Scheduled-task settings and logs |
| Playlists and collections | Metadata edits and images stored only in Jellyfin's database. Deadfin scans and fetches them again |
| Library definitions (with `--jellyfin-root`): names, paths, types, library options | Activity log |

A Jellyfin user whose name already exists in Deadfin is merged into that
Deadfin user, which keeps its Deadfin ID, password and policy. The Jellyfin
user's device sign-ins and watch state go to the merged user. Other users get
new IDs. Clients that cached the old user ID may need one fresh sign-in.

Deadfin matches watch state to the items that its own scan found, by
provider IDs (IMDb, TMDb, TVDb) and then by path. That is why you scan
before you import watch state.

## Steps

The paths below assume the compose file in [install.md](install.md) and a
stock Jellyfin Docker install with its config at `./jellyfin/config`.

1. **Stop Jellyfin** so that its database is consistent, and copy it:

   ```bash
   docker compose stop jellyfin
   mkdir -p ./import
   cp ./jellyfin/config/data/jellyfin.db* ./import/
   cp -r ./jellyfin/config/root/default ./import/root-default
   docker compose start jellyfin   # optional: keep Jellyfin running beside Deadfin
   ```

   On a package install the files are in `/var/lib/jellyfin/data/` and
   `/var/lib/jellyfin/root/default/`.

2. **Mount your media at the same paths that Jellyfin used.** Library paths
   in `root-default` are Jellyfin's paths. If Jellyfin saw `/data/movies`,
   mount the same folder at `/data/movies` in Deadfin. The import skips a
   library whose path is missing and tells you.

3. **Start Deadfin once and finish the wizard** (see
   [install.md](install.md)). Use an admin name that does not exist in
   Jellyfin, or the same name as your Jellyfin admin if you want the two
   accounts merged. Then stop Deadfin.

4. **Copy the library definitions** (a dry run first):

   ```bash
   docker compose run --rm -v ./import:/import:ro deadfin \
     import-jellyfin --jellyfin-db /import/jellyfin.db --jellyfin-root /import/root-default --dry-run
   docker compose run --rm -v ./import:/import:ro deadfin \
     import-jellyfin --jellyfin-db /import/jellyfin.db --jellyfin-root /import/root-default
   ```

5. **Start Deadfin and scan.** Start the scan by hand: Dashboard →
   Libraries → Scan All Libraries. The server does not scan when it starts.
   As in Jellyfin, the scheduled task Scan Media Library first runs one hour
   after the server starts, then 12 hours after its last run. Wait until the
   scan is done. With `TMDB_API_KEY` set, let the metadata refresh finish too: watch
   state for movies and shows matches best when the items have provider IDs.

6. **Stop Deadfin and import again.** This run matches watch state to the
   scanned items:

   ```bash
   docker compose stop deadfin
   docker compose run --rm -v ./import:/import:ro deadfin \
     import-jellyfin --jellyfin-db /import/jellyfin.db --jellyfin-root /import/root-default
   docker compose start deadfin
   ```

   The import is safe to run again. Run it again after a later scan finds
   more items.

7. **Check.** Sign in with a client that was signed in to Jellyfin. Check
   Continue Watching, Next Up, favorites and a playlist.

8. **Connect your other tools.** Sonarr, Radarr, Seerr and Bazarr: see
   [integrations.md](integrations.md). Seerr needs its user IDs changed to
   the Deadfin IDs.

## If something is missing

- Items not matched: the command prints the watch-state rows matched (by key
  and by path) and the rows not imported, with the unmatched count under each
  library path. A large unmatched count usually means a path mismatch
  (step 2) or a scan that was not done (step 5).
- Library type rejected: Deadfin accepts only the documented
  `CollectionType` values (D-26). Fix the type in Jellyfin's `options.xml`
  copy or add the library by hand.
- Report the counts and the command output (see
  [reporting-bugs.md](reporting-bugs.md)). Remove user names if you share
  the output in public.

## Going back to Jellyfin

Stop Deadfin and start Jellyfin. Jellyfin has the state from before the
import. Playlists and collections that you made in Deadfin are not visible
to Jellyfin (D-42, D-43).
