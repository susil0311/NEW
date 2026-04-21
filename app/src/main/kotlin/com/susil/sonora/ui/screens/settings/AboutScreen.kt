package com.susil.sonora.ui.screens.settings

import android.os.Build
import androidx.compose.animation.core.Animatable
import androidx.compose.animation.core.FastOutSlowInEasing
import androidx.compose.animation.core.LinearEasing
import androidx.compose.animation.core.RepeatMode
import androidx.compose.animation.core.animateFloat
import androidx.compose.animation.core.animateFloatAsState
import androidx.compose.animation.core.infiniteRepeatable
import androidx.compose.animation.core.rememberInfiniteTransition
import androidx.compose.animation.core.tween
import androidx.compose.foundation.Image
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.RowScope
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.ArrowBack
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TopAppBar
import androidx.compose.material3.TopAppBarDefaults
import androidx.compose.material3.TopAppBarScrollBehavior
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.draw.scale
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.asComposeRenderEffect
import androidx.compose.ui.graphics.graphicsLayer
import androidx.compose.ui.layout.ContentScale
import androidx.compose.ui.platform.LocalUriHandler
import androidx.compose.ui.res.painterResource
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.dp
import androidx.compose.ui.zIndex
import androidx.navigation.NavController
import com.susil.sonora.BuildConfig
import com.susil.sonora.R
import kotlinx.coroutines.delay

@Composable
private fun SonoraAnimatedLogo(
    modifier: Modifier = Modifier,
    size: Dp = 72.dp,
) {
    val barHeightFractions = listOf(0.30f, 0.52f, 0.70f, 0.52f, 0.30f)
    val staggerMs = 150
    val scales = remember { List(5) { Animatable(0f) } }

    LaunchedEffect(Unit) {
        scales.forEachIndexed { i, anim ->
            delay(i * staggerMs.toLong())
            anim.animateTo(
                targetValue = 1f,
                animationSpec = tween(durationMillis = 400, easing = FastOutSlowInEasing),
            )
        }
    }

    val infiniteTransition = rememberInfiniteTransition(label = "sonoraPulse")
    val pulseValues =
        barHeightFractions.mapIndexed { i, _ ->
            infiniteTransition.animateFloat(
                initialValue = 0.55f,
                targetValue = 1f,
                animationSpec = infiniteRepeatable(
                    animation = tween(
                        durationMillis = 700,
                        delayMillis = i * staggerMs,
                        easing = FastOutSlowInEasing,
                    ),
                    repeatMode = RepeatMode.Reverse,
                ),
                label = "bar$i",
            )
        }

    val barColors =
        listOf(
            Color(0xFFc084fc),
            Color(0xFF9d6ff5),
            Color(0xFF818cf8),
            Color(0xFF5ba8f5),
            Color(0xFF38bdf8),
        )

    Box(
        modifier = modifier.size(size),
        contentAlignment = Alignment.Center,
    ) {
        Box(
            modifier = Modifier
                .matchParentSize()
                .background(
                    brush = Brush.linearGradient(
                        colors = listOf(Color(0xFF1a0533), Color(0xFF0d1a3a), Color(0xFF001a2e)),
                    ),
                    shape = RoundedCornerShape(size * 0.26f),
                ),
        )

        Row(
            horizontalArrangement = Arrangement.spacedBy(size * 0.04f),
            verticalAlignment = Alignment.CenterVertically,
            modifier = Modifier.padding(horizontal = size * 0.14f),
        ) {
            barHeightFractions.forEachIndexed { i, fraction ->
                val entranceScale = scales[i].value
                val pulseScale by pulseValues[i]
                val barMaxH = size * fraction * 0.72f
                val barH = barMaxH * pulseScale * entranceScale
                val barW = size * 0.085f

                Box(
                    modifier = Modifier
                        .width(barW)
                        .height(barH.coerceAtLeast(2.dp))
                        .background(
                            brush = Brush.verticalGradient(
                                colors = listOf(barColors[i], barColors[i].copy(alpha = 0.6f)),
                            ),
                            shape = RoundedCornerShape(50),
                        ),
                )
            }
        }
    }
}

@Composable
private fun RowScope.SegmentedButton(
    label: String,
    iconRes: Int,
    onClick: () -> Unit,
) {
    Column(
        horizontalAlignment = Alignment.CenterHorizontally,
        verticalArrangement = Arrangement.Center,
        modifier = Modifier
            .weight(1f)
            .height(72.dp)
            .clickable(onClick = onClick),
    ) {
        Icon(
            painter = painterResource(iconRes),
            contentDescription = null,
            tint = MaterialTheme.colorScheme.onSurface,
            modifier = Modifier.size(22.dp),
        )
        Spacer(Modifier.height(4.dp))
        Text(
            text = label,
            style = MaterialTheme.typography.labelMedium,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
    }
}

@Composable
private fun SectionTitle(title: String) {
    Text(
        text = title,
        style = MaterialTheme.typography.labelLarge,
        color = MaterialTheme.colorScheme.primary,
        fontWeight = FontWeight.Bold,
        modifier = Modifier
            .fillMaxWidth()
            .padding(horizontal = 4.dp, vertical = 4.dp),
        textAlign = TextAlign.Start,
    )
}

@Composable
private fun AboutCard(
    modifier: Modifier = Modifier,
    onClick: (() -> Unit)? = null,
    content: @Composable () -> Unit,
) {
    Surface(
        modifier = modifier
            .fillMaxWidth()
            .then(if (onClick != null) Modifier.clickable(onClick = onClick) else Modifier),
        shape = RoundedCornerShape(28.dp),
        color = MaterialTheme.colorScheme.surfaceContainerHigh,
        tonalElevation = 1.dp,
    ) {
        content()
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun AboutScreen(
    navController: NavController,
    scrollBehavior: TopAppBarScrollBehavior,
) {
    val uriHandler = LocalUriHandler.current
    val colorScheme = MaterialTheme.colorScheme

    var logoAnimated by remember { mutableStateOf(false) }
    val logoScale by animateFloatAsState(
        targetValue = if (logoAnimated) 1f else 0.8f,
        animationSpec = tween(durationMillis = 350),
        label = "logoScale",
    )

    LaunchedEffect(Unit) { logoAnimated = true }

    Scaffold(
        modifier = Modifier.fillMaxSize(),
        containerColor = colorScheme.background,
        topBar = {
            Box {
                Box(
                    modifier = Modifier
                        .fillMaxWidth()
                        .height(100.dp)
                        .zIndex(10f)
                        .then(
                            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S) {
                                Modifier.graphicsLayer {
                                    renderEffect = android.graphics.RenderEffect.createBlurEffect(
                                        25f,
                                        25f,
                                        android.graphics.Shader.TileMode.CLAMP,
                                    ).asComposeRenderEffect()
                                }
                            } else {
                                Modifier
                            },
                        )
                        .background(
                            brush = Brush.verticalGradient(
                                colors = listOf(
                                    colorScheme.surface.copy(alpha = 0.98f),
                                    colorScheme.surface.copy(alpha = 0.92f),
                                    Color.Transparent,
                                ),
                            ),
                        ),
                )

                TopAppBar(
                    title = {
                        Text(
                            text = "About",
                            fontWeight = FontWeight.Bold,
                        )
                    },
                    navigationIcon = {
                        Box(
                            modifier = Modifier
                                .clip(CircleShape)
                                .clickable(onClick = navController::navigateUp)
                                .padding(8.dp),
                        ) {
                            Icon(
                                imageVector = Icons.AutoMirrored.Filled.ArrowBack,
                                contentDescription = "Back",
                            )
                        }
                    },
                    colors = TopAppBarDefaults.topAppBarColors(
                        containerColor = Color.Transparent,
                        scrolledContainerColor = colorScheme.surfaceContainer,
                    ),
                    modifier = Modifier.zIndex(11f),
                    scrollBehavior = scrollBehavior,
                )
            }
        },
    ) { paddingValues ->
        LazyColumn(
            modifier = Modifier
                .fillMaxSize()
                .padding(paddingValues),
            contentPadding = PaddingValues(start = 16.dp, end = 16.dp, bottom = 100.dp),
            horizontalAlignment = Alignment.CenterHorizontally,
        ) {
            item {
                Spacer(Modifier.height(16.dp))

                AboutCard {
                    Column(
                        horizontalAlignment = Alignment.CenterHorizontally,
                        modifier = Modifier
                            .fillMaxWidth()
                            .padding(vertical = 28.dp),
                    ) {
                        SonoraAnimatedLogo(
                            size = 72.dp,
                            modifier = Modifier.scale(logoScale),
                        )

                        Spacer(Modifier.height(14.dp))

                        Text(
                            text = "Sonora",
                            style = MaterialTheme.typography.headlineMedium.copy(
                                fontWeight = FontWeight.Black,
                            ),
                            color = colorScheme.onSurface,
                        )

                        Spacer(Modifier.height(8.dp))

                        Surface(
                            shape = CircleShape,
                            color = colorScheme.secondaryContainer,
                        ) {
                            Text(
                                text = "v${BuildConfig.VERSION_NAME}",
                                style = MaterialTheme.typography.labelMedium,
                                fontWeight = FontWeight.Bold,
                                color = colorScheme.onSecondaryContainer,
                                modifier = Modifier.padding(horizontal = 12.dp, vertical = 6.dp),
                            )
                        }
                    }
                }

                Spacer(Modifier.height(24.dp))

                HorizontalDivider(
                    modifier = Modifier.padding(horizontal = 32.dp),
                    thickness = 2.dp,
                    color = colorScheme.onSurface.copy(alpha = 0.15f),
                )

                Spacer(Modifier.height(28.dp))
            }

            item {
                SectionTitle("Developer")
                Spacer(Modifier.height(16.dp))

                Box(
                    modifier = Modifier
                        .size(160.dp)
                        .clip(RoundedCornerShape(28.dp))
                        .background(colorScheme.surfaceContainerHighest)
                        .scale(logoScale),
                    contentAlignment = Alignment.Center,
                ) {
                    Image(
                        painter = painterResource(R.drawable.susil_avatar),
                        contentDescription = "Susil Kumar",
                        contentScale = ContentScale.Crop,
                        modifier = Modifier.fillMaxSize(),
                    )
                }

                Spacer(Modifier.height(16.dp))

                Text(
                    text = "Susil Kumar",
                    style = MaterialTheme.typography.headlineSmall,
                    fontWeight = FontWeight.Bold,
                    color = colorScheme.onSurface,
                )

                Spacer(Modifier.height(16.dp))

                AboutCard {
                    Row(modifier = Modifier.fillMaxWidth()) {
                        SegmentedButton(
                            label = "Instagram",
                            iconRes = R.drawable.instagram,
                            onClick = {
                                uriHandler.openUri("https://www.instagram.com/imsusil25.exe")
                            },
                        )

                        Box(
                            modifier = Modifier
                                .width(1.dp)
                                .height(72.dp)
                                .background(colorScheme.outlineVariant.copy(alpha = 0.5f)),
                        )

                        SegmentedButton(
                            label = "Telegram",
                            iconRes = R.drawable.telegram,
                            onClick = { uriHandler.openUri("https://t.me/mikey3op") },
                        )
                    }
                }

                Spacer(Modifier.height(24.dp))
            }

            item {
                SectionTitle("Support Development")
                Spacer(Modifier.height(8.dp))

                AboutCard(
                    onClick = {
                        uriHandler.openUri(
                            "https://intradeus.github.io/http-protocol-redirector/?r=upi://pay?pa=iamsusil@fam&pn=Susil%20Kumar&am=&tn=Thank%20You%20so%20much%20for%20this%20support",
                        )
                    },
                ) {
                    Row(
                        verticalAlignment = Alignment.CenterVertically,
                        modifier = Modifier.padding(horizontal = 16.dp, vertical = 20.dp),
                    ) {
                        Image(
                            painter = painterResource(R.drawable.upi),
                            contentDescription = "UPI",
                            modifier = Modifier
                                .width(80.dp)
                                .height(36.dp),
                        )

                        Spacer(Modifier.weight(1f))

                        Column(horizontalAlignment = Alignment.End) {
                            Text(
                                text = "Tap to Support",
                                style = MaterialTheme.typography.titleSmall,
                                fontWeight = FontWeight.Bold,
                                color = colorScheme.onSurface,
                            )

                            Spacer(Modifier.height(2.dp))

                            Text(
                                text = "via UPI (India)",
                                style = MaterialTheme.typography.bodySmall,
                                color = colorScheme.onSurfaceVariant,
                            )
                        }
                    }
                }

                Spacer(Modifier.height(28.dp))

                HorizontalDivider(
                    modifier = Modifier.padding(horizontal = 32.dp),
                    thickness = 1.dp,
                    color = colorScheme.onSurface.copy(alpha = 0.08f),
                )

                Spacer(Modifier.height(28.dp))
            }

            item {
                Text(
                    text = "♥ Made with love for music lovers",
                    style = MaterialTheme.typography.bodySmall,
                    color = colorScheme.onSurface.copy(alpha = 0.4f),
                    textAlign = TextAlign.Center,
                    modifier = Modifier
                        .fillMaxWidth()
                        .padding(horizontal = 16.dp),
                )

                Spacer(Modifier.height(24.dp))
            }
        }
    }
}
