/*
 * Sonora Project Original (2026)
 * Chartreux Westia (github.com/koiverse)
 * Licensed Under GPL-3.0 | see git history for contributors
 * Don't remove this copyright holder!
 */

package com.susil.sonora.ui.menu

import com.susil.sonora.db.entities.PlaylistSongMap
import com.susil.sonora.innertube.YouTube

suspend fun removeSongFromRemotePlaylist(
    playlistBrowseId: String,
    playlistSongMap: PlaylistSongMap,
): Result<Unit> = runCatching {
    val setVideoIds =
        playlistSongMap.setVideoId?.let(::listOf)
            ?: YouTube.playlistEntrySetVideoIds(playlistBrowseId, playlistSongMap.songId).getOrThrow()

    setVideoIds
        .distinct()
        .forEach { setVideoId ->
            YouTube.removeFromPlaylist(playlistBrowseId, playlistSongMap.songId, setVideoId).getOrThrow()
        }
}
