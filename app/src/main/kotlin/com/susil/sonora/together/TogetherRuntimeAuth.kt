/*
 * Sonora Project Original (2026)
 * Chartreux Westia (github.com/koiverse)
 * Licensed Under GPL-3.0 | see git history for contributors
 * Don't remove this copyright holder!
 */

package com.susil.sonora.together

import androidx.datastore.core.DataStore
import androidx.datastore.preferences.core.Preferences
import androidx.datastore.preferences.core.edit
import com.susil.sonora.constants.TogetherOnlineRuntimeTokenExpiresAtKey
import com.susil.sonora.constants.TogetherOnlineRuntimeTokenKey
import com.susil.sonora.utils.getAsync
import java.net.URI

object TogetherRuntimeAuth {
    private const val RefreshSkewSeconds: Long = 45
    private const val DefaultFallbackTokenTtlSeconds: Long = 30 * 60

    suspend fun tokenOrNull(
        dataStore: DataStore<Preferences>,
        baseUrl: String,
        fallbackStaticToken: String? = null,
        scopes: List<String> = listOf("together:rw", "canvas:read"),
    ): String? {
        val nowSeconds = System.currentTimeMillis() / 1000L

        val cachedToken =
            dataStore.getAsync(TogetherOnlineRuntimeTokenKey)
                ?.trim()
                .orEmpty()
        val cachedExpiresAt = dataStore.getAsync(TogetherOnlineRuntimeTokenExpiresAtKey, 0L)

        if (cachedToken.isNotBlank() && cachedExpiresAt > nowSeconds + RefreshSkewSeconds) {
            return cachedToken
        }

        val staticToken = fallbackStaticToken?.trim()?.takeIf { it.isNotBlank() }
        if (staticToken != null) {
            val expiresAt = nowSeconds + DefaultFallbackTokenTtlSeconds
            dataStore.edit { prefs ->
                prefs[TogetherOnlineRuntimeTokenKey] = staticToken
                prefs[TogetherOnlineRuntimeTokenExpiresAtKey] = expiresAt
            }
            return staticToken
        }

        val issued =
            runCatching {
                TogetherOnlineApi(baseUrl = baseUrl).issueRuntimeToken(scopes = scopes)
            }.getOrNull()
                ?: return null

        val token = issued.token.trim().takeIf { it.isNotBlank() } ?: return null
        val expiresAt =
            when {
                (issued.expiresAt ?: 0L) > nowSeconds -> issued.expiresAt!!
                (issued.expiresIn ?: 0L) > 0L -> nowSeconds + issued.expiresIn!!
                else -> nowSeconds + DefaultFallbackTokenTtlSeconds
            }

        dataStore.edit { prefs ->
            prefs[TogetherOnlineRuntimeTokenKey] = token
            prefs[TogetherOnlineRuntimeTokenExpiresAtKey] = expiresAt
        }

        return token
    }

    fun canvasProxyBaseUrl(baseUrl: String): String {
        val trimmed = baseUrl.trim().trimEnd('/')
        val uri = runCatching { URI(trimmed) }.getOrNull()
        val path = uri?.path?.trim().orEmpty().trimEnd('/')
        val normalizedPath = if (path.endsWith("/v1")) "$path/canvas" else "$path/v1/canvas"

        return if (uri != null && !uri.scheme.isNullOrBlank() && !uri.host.isNullOrBlank()) {
            val scheme = uri.scheme.trim().lowercase()
            val host = uri.host.trim()
            val port = if (uri.port != -1 && uri.port != 80 && uri.port != 443) ":${uri.port}" else ""
            "$scheme://$host$port$normalizedPath"
        } else {
            if (trimmed.endsWith("/v1")) "$trimmed/canvas" else "$trimmed/v1/canvas"
        }
    }
}
