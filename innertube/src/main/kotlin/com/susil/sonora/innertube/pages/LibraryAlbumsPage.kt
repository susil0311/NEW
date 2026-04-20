/*
 * Sonora Project Original (2026)
 * Chartreux Westia (github.com/koiverse)
 * Licensed Under GPL-3.0 | see git history for contributors
 * Don't remove this copyright holder!
 */




package com.susil.sonora.innertube.pages

import com.susil.sonora.innertube.models.Album
import com.susil.sonora.innertube.models.AlbumItem
import com.susil.sonora.innertube.models.Artist
import com.susil.sonora.innertube.models.ArtistItem
import com.susil.sonora.innertube.models.MusicResponsiveListItemRenderer
import com.susil.sonora.innertube.models.MusicTwoRowItemRenderer
import com.susil.sonora.innertube.models.PlaylistItem
import com.susil.sonora.innertube.models.SongItem
import com.susil.sonora.innertube.models.YTItem
import com.susil.sonora.innertube.models.oddElements
import com.susil.sonora.innertube.utils.parseTime

data class LibraryAlbumsPage(
    val albums: List<AlbumItem>,
    val continuation: String?,
) {
    companion object {
        fun fromMusicTwoRowItemRenderer(renderer: MusicTwoRowItemRenderer): AlbumItem? {
            val browseId = renderer.navigationEndpoint.browseEndpoint?.browseId ?: return null
            val playlistId = renderer.thumbnailOverlay?.musicItemThumbnailOverlayRenderer?.content
                ?.musicPlayButtonRenderer?.playNavigationEndpoint
                ?.watchPlaylistEndpoint?.playlistId
                ?: renderer.menu?.menuRenderer?.items?.firstOrNull()
                    ?.menuNavigationItemRenderer?.navigationEndpoint
                    ?.watchPlaylistEndpoint?.playlistId
                ?: browseId.removePrefix("MPREb_").let { "OLAK5uy_$it" }

            return AlbumItem(
                browseId = browseId,
                playlistId = playlistId,
                title = renderer.title.runs?.firstOrNull()?.text ?: return null,
                artists = null,
                year = renderer.subtitle?.runs?.lastOrNull()?.text?.toIntOrNull(),
                thumbnail = renderer.thumbnailRenderer.musicThumbnailRenderer?.getThumbnailUrl() ?: return null,
                explicit = renderer.subtitleBadges?.find {
                    it.musicInlineBadgeRenderer?.icon?.iconType == "MUSIC_EXPLICIT_BADGE"
                } != null
            )
        }
    }
}
