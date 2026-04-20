/*
 * Sonora Project Original (2026)
 * Chartreux Westia (github.com/koiverse)
 * Licensed Under GPL-3.0 | see git history for contributors
 * Don't remove this copyright holder!
 */




package com.susil.sonora.playback.queues

import androidx.media3.common.MediaItem
import com.susil.sonora.innertube.YouTube
import com.susil.sonora.innertube.models.WatchEndpoint
import com.susil.sonora.db.entities.AlbumWithSongs
import com.susil.sonora.extensions.toMediaItem
import com.susil.sonora.models.MediaMetadata
import kotlinx.coroutines.Dispatchers.IO
import kotlinx.coroutines.withContext

class LocalAlbumRadio(
    private val albumWithSongs: AlbumWithSongs,
    private val startIndex: Int = 0,
) : Queue {
    override val preloadItem: MediaMetadata? = null

    private lateinit var playlistId: String
    private val endpoint: WatchEndpoint
        get() = WatchEndpoint(
            playlistId = playlistId,
            params = "wAEB"
        )

    private var continuation: String? = null
    private var firstTimeLoaded: Boolean = false

    override suspend fun getInitialStatus(): Queue.Status = withContext(IO) {
        Queue.Status(
            title = albumWithSongs.album.title,
            items = albumWithSongs.songs.map { it.toMediaItem() },
            mediaItemIndex = startIndex
        )
    }

    override fun hasNextPage(): Boolean = !firstTimeLoaded || continuation != null

    override suspend fun nextPage(): List<MediaItem> = withContext(IO) {
        if (!firstTimeLoaded) {
            playlistId = YouTube.album(albumWithSongs.album.id).getOrThrow().album.playlistId
            val nextResult = YouTube.next(endpoint, continuation).getOrThrow()
            continuation = nextResult.continuation
            firstTimeLoaded = true
            return@withContext nextResult.items.subList(
                albumWithSongs.songs.size,
                nextResult.items.size
            ).map { it.toMediaItem() }
        }
        val nextResult = YouTube.next(endpoint, continuation).getOrThrow()
        continuation = nextResult.continuation
        nextResult.items.map { it.toMediaItem() }
    }
}
