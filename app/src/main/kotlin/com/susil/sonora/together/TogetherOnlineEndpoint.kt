/*
 * Sonora Project Original (2026)
 * Chartreux Westia (github.com/koiverse)
 * Licensed Under GPL-3.0 | see git history for contributors
 * Don't remove this copyright holder!
 *
 * CHANGES (Render + Supabase edition)
 *  • RENDER_BASE_URL is now the primary hard-coded endpoint.
 *  • The GitHub text-file mechanism still works as a dynamic override
 *    (useful if you ever change your Render URL or add a custom domain).
 *  • Resolution order:
 *      1. DataStore user override  (Settings screen, if you expose one)
 *      2. GitHub text-file fetch   (cached 6 h)
 *      3. RENDER_BASE_URL          ← new hard-coded fallback
 */

package com.susil.sonora.together

import androidx.datastore.core.DataStore
import androidx.datastore.preferences.core.Preferences
import androidx.datastore.preferences.core.edit
import io.ktor.client.HttpClient
import io.ktor.client.engine.okhttp.OkHttp
import io.ktor.client.request.get
import io.ktor.client.statement.bodyAsText
import java.net.URI
import java.util.concurrent.TimeUnit
import com.susil.sonora.constants.TogetherOnlineEndpointCacheKey
import com.susil.sonora.constants.TogetherOnlineEndpointLastCheckedAtKey
import com.susil.sonora.constants.TogetherOnlineEndpointOverrideKey
import com.susil.sonora.utils.getAsync

object TogetherOnlineEndpoint {

    /**
     * ╔══════════════════════════════════════════════════════════╗
     * ║  SET THIS to your Render service URL after deployment.  ║
     * ║  Example: "https://sonora-together.onrender.com"        ║
     * ╚══════════════════════════════════════════════════════════╝
     */
    private const val RENDER_BASE_URL = "https://sonora-together.onrender.com"

    // Dynamic source URLs (optional – lets you change the server URL
    // without shipping a new APK by updating a text file on GitHub).
    private const val EndpointSourceUrl =
        "https://raw.githubusercontent.com/koiverse/Sonora/refs/heads/dev/SonoraKoiverseServer.txt"

    private const val EndpointSourceUrlFallback =
        "https://raw.githubusercontent.com/koiverse/Sonora/main/SonoraKoiverseServer.txt"

    private const val CacheTtlMs: Long = 6 * 60 * 60 * 1000L  // 6 hours

    private val httpClient =
        HttpClient(OkHttp) {
            engine {
                config {
                    connectTimeout(12, TimeUnit.SECONDS)
                    readTimeout(12, TimeUnit.SECONDS)
                    writeTimeout(12, TimeUnit.SECONDS)
                    retryOnConnectionFailure(true)
                }
            }
        }

    /**
     * Returns the base URL to use for the Together server, or null
     * if nothing is available (which should never happen once
     * RENDER_BASE_URL is set).
     *
     * Resolution order:
     *   1. Manual override stored in DataStore (e.g. from Settings screen)
     *   2. GitHub text-file (dynamic, cached 6 h)
     *   3. RENDER_BASE_URL  (hard-coded Render service)
     */
    suspend fun baseUrlOrNull(
        dataStore: DataStore<Preferences>,
    ): String? {
        val now = System.currentTimeMillis()

        // 1. Manual override
        val override =
            dataStore.getAsync(TogetherOnlineEndpointOverrideKey)
                ?.trim()
                .orEmpty()
                .trimEnd('/')
        if (override.isNotBlank() && isValidHttpBaseUrl(override)) return override

        // 2. GitHub text-file (cached)
        val cached = dataStore.getAsync(TogetherOnlineEndpointCacheKey)?.trim().orEmpty()
        val lastCheckedAt = dataStore.getAsync(TogetherOnlineEndpointLastCheckedAtKey, 0L)

        if (cached.isNotBlank() && now - lastCheckedAt < CacheTtlMs) return cached

        val fetched = fetchEndpointFromSourceOrNull()
        if (fetched != null) {
            dataStore.edit { prefs ->
                prefs[TogetherOnlineEndpointCacheKey] = fetched
                prefs[TogetherOnlineEndpointLastCheckedAtKey] = now
            }
            return fetched
        }

        // Update last-checked even if fetch failed, so we don't hammer on error
        dataStore.edit { prefs ->
            prefs[TogetherOnlineEndpointLastCheckedAtKey] = now
        }

        // Return stale cache if available
        if (cached.isNotBlank()) return cached

        // 3. Hard-coded Render URL
        return RENDER_BASE_URL.takeIf { it.isNotBlank() && isValidHttpBaseUrl(it) }
    }

    private suspend fun fetchEndpointFromSourceOrNull(): String? {
        val urls = listOf(EndpointSourceUrl, EndpointSourceUrlFallback)
        for (source in urls) {
            val text =
                runCatching { httpClient.get(source).bodyAsText() }
                    .getOrNull()
                    ?.trim()
                    .orEmpty()
            if (text.isBlank()) continue

            val candidate =
                text.lineSequence()
                    .map { it.trim() }
                    .firstOrNull { it.isNotBlank() }
                    ?.trimEnd('/')
                    ?: continue

            if (isValidHttpBaseUrl(candidate)) return candidate
        }
        return null
    }

    private fun isValidHttpBaseUrl(candidate: String): Boolean {
        val uri = runCatching { URI(candidate) }.getOrNull() ?: return false
        val scheme = uri.scheme?.trim()?.lowercase()
        if (scheme != "http" && scheme != "https") return false
        val host = uri.host?.trim().orEmpty()
        return host.isNotBlank()
    }

    fun onlineWebSocketUrlOrNull(
        rawWsUrl: String,
        baseUrl: String,
    ): String? {
        val derived = deriveWebSocketUrlFromBaseUrl(baseUrl) ?: return null
        val normalized = normalizeWebSocketUrl(rawWsUrl, baseUrl) ?: return derived

        val host =
            runCatching { URI(normalized).host }.getOrNull()?.trim()?.lowercase()
                ?: return derived
        if (host == "localhost" || host == "127.0.0.1" || host == "0.0.0.0") return derived

        val baseHost =
            runCatching { URI(baseUrl.trim()).host }.getOrNull()?.trim()?.lowercase()
        if (baseHost != null && isIpv4Address(baseHost) && !isIpv4Address(host)) return derived

        return normalized
    }

    private fun isIpv4Address(host: String): Boolean {
        val parts = host.split('.')
        if (parts.size != 4) return false
        return parts.all { part ->
            val n = part.toIntOrNull() ?: return@all false
            n in 0..255 && part == n.toString()
        }
    }

    private fun deriveWebSocketUrlFromBaseUrl(baseUrl: String): String? {
        val uri = runCatching { URI(baseUrl.trim()) }.getOrNull() ?: return null
        val host = uri.host?.trim()?.ifBlank { null } ?: return null
        val scheme = uri.scheme?.trim()?.lowercase()
        val wsScheme = if (scheme == "https") "wss" else "ws"
        val portPart =
            if (uri.port != -1 && uri.port != 80 && uri.port != 443) ":${uri.port}" else ""
        val normalizedPath =
            uri.path
                ?.trim()
                ?.trimEnd('/')
                .orEmpty()
                .let { if (it.endsWith("/v1")) it else "$it/v1" }
        return "$wsScheme://$host$portPart$normalizedPath/together/ws"
    }

    private fun normalizeWebSocketUrl(raw: String, baseUrl: String): String? {
        val trimmed = raw.trim()
        if (trimmed.isBlank()) return null
        if (trimmed.startsWith("ws://") || trimmed.startsWith("wss://")) return trimmed
        if (trimmed.startsWith("http://")) return "ws://${trimmed.removePrefix("http://")}"
        if (trimmed.startsWith("https://")) return "wss://${trimmed.removePrefix("https://")}"
        if (trimmed.startsWith("/")) {
            val baseUri = runCatching { URI(baseUrl.trim()) }.getOrNull() ?: return null
            val host = baseUri.host?.trim()?.ifBlank { null } ?: return null
            val scheme = baseUri.scheme?.trim()?.lowercase()
            val wsScheme = if (scheme == "https") "wss" else "ws"
            val portPart =
                if (baseUri.port != -1 && baseUri.port != 80 && baseUri.port != 443) ":${baseUri.port}" else ""
            val basePath = baseUri.path?.trim()?.trimEnd('/').orEmpty()
            return "$wsScheme://$host$portPart$basePath$trimmed"
        }
        val baseScheme =
            runCatching { URI(baseUrl.trim()).scheme?.trim()?.lowercase() }.getOrNull()
        val wsScheme = if (baseScheme == "https") "wss" else "ws"
        return "$wsScheme://$trimmed"
    }
}
