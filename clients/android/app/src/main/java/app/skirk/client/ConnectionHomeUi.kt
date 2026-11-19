package app.skirk.client

import androidx.compose.foundation.Image
import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.BoxWithConstraints
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.rounded.ArrowBack
import androidx.compose.material.icons.outlined.AccountCircle
import androidx.compose.material.icons.outlined.ErrorOutline
import androidx.compose.material.icons.outlined.Settings
import androidx.compose.material.icons.outlined.Shield
import androidx.compose.material.icons.outlined.WifiTethering
import androidx.compose.material.icons.rounded.CheckCircle
import androidx.compose.material.icons.rounded.PowerSettingsNew
import androidx.compose.material3.Button
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TopAppBar
import androidx.compose.material3.TopAppBarDefaults
import androidx.compose.material3.darkColorScheme
import androidx.compose.material3.lightColorScheme
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.LocalDensity
import androidx.compose.ui.res.painterResource
import androidx.compose.ui.semantics.LiveRegionMode
import androidx.compose.ui.semantics.heading
import androidx.compose.ui.semantics.liveRegion
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import java.text.NumberFormat

// Keep the brand monochrome. Tonal green is reserved for live status and the
// matching dark role remains distinct from the normal foreground.
private val LightColors = lightColorScheme(
    primary = Color(0xFF202723),
    onPrimary = Color.White,
    primaryContainer = Color(0xFFDCEFE2),
    onPrimaryContainer = Color(0xFF214C36),
    secondary = Color(0xFF46574D),
    onSecondary = Color.White,
    secondaryContainer = Color(0xFFE5ECE6),
    onSecondaryContainer = Color(0xFF26362C),
    tertiary = Color(0xFF4A5750),
    onTertiary = Color.White,
    tertiaryContainer = Color(0xFFE4ECE5),
    onTertiaryContainer = Color(0xFF26352B),
    background = Color(0xFFFBFCF9),
    onBackground = Color(0xFF1C241F),
    surface = Color(0xFFFBFCF9),
    onSurface = Color(0xFF1C241F),
    surfaceVariant = Color(0xFFE8EDE7),
    onSurfaceVariant = Color(0xFF4C5950),
    surfaceContainerLow = Color(0xFFF2F5F0),
    surfaceContainer = Color(0xFFEFF2ED),
    surfaceContainerHigh = Color(0xFFE9EDE7),
    surfaceContainerHighest = Color(0xFFE3E8E2),
    outline = Color(0xFF6F7B71),
    outlineVariant = Color(0xFFD8DFD7),
    inverseSurface = Color(0xFF323B34),
    inverseOnSurface = Color(0xFFF0F5EF),
    inversePrimary = Color(0xFFC6D4C9),
    surfaceTint = Color(0xFF46574D),
)

private val DarkColors = darkColorScheme(
    primary = Color(0xFFE6EEE7),
    onPrimary = Color(0xFF1E2A22),
    primaryContainer = Color(0xFF233E2E),
    onPrimaryContainer = Color(0xFFBCE3C8),
    secondary = Color(0xFFC3CEC4),
    onSecondary = Color(0xFF263229),
    secondaryContainer = Color(0xFF303E34),
    onSecondaryContainer = Color(0xFFDCE8DE),
    tertiary = Color(0xFFC3D0C6),
    onTertiary = Color(0xFF28332A),
    tertiaryContainer = Color(0xFF343F36),
    onTertiaryContainer = Color(0xFFDDE9DF),
    background = Color(0xFF151916),
    onBackground = Color(0xFFE4EAE4),
    surface = Color(0xFF151916),
    onSurface = Color(0xFFE4EAE4),
    surfaceVariant = Color(0xFF29312B),
    onSurfaceVariant = Color(0xFFBCC8BE),
    surfaceContainerLow = Color(0xFF1D231E),
    surfaceContainer = Color(0xFF212722),
    surfaceContainerHigh = Color(0xFF29312B),
    surfaceContainerHighest = Color(0xFF323C34),
    outline = Color(0xFF8C9B90),
    outlineVariant = Color(0xFF39473D),
    inverseSurface = Color(0xFFDCE6DD),
    inverseOnSurface = Color(0xFF27322A),
    inversePrimary = Color(0xFF4A5F50),
    surfaceTint = Color(0xFFC3CEC4),
)

@Composable
internal fun SkirkTheme(content: @Composable () -> Unit) {
    MaterialTheme(colorScheme = if (isSystemInDarkTheme()) DarkColors else LightColors, content = content)
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
internal fun SkirkTopBar(page: SkirkPage, onBack: () -> Unit) {
    TopAppBar(
        title = {
            if (page == SkirkPage.Home) {
                Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(10.dp)) {
                    Surface(shape = RoundedCornerShape(8.dp), color = Color.White, modifier = Modifier.size(28.dp)) {
                        Image(painterResource(R.drawable.logo_mark), contentDescription = null, modifier = Modifier.padding(4.dp))
                    }
                    Text(page.title, style = MaterialTheme.typography.titleLarge, fontWeight = FontWeight.SemiBold)
                }
            } else {
                Text(page.title, modifier = Modifier.semantics { heading() })
            }
        },
        navigationIcon = if (page == SkirkPage.Home) ({}) else ({
            IconButton(onClick = onBack) { Icon(Icons.AutoMirrored.Rounded.ArrowBack, contentDescription = "Back") }
        }),
        colors = TopAppBarDefaults.topAppBarColors(
            containerColor = MaterialTheme.colorScheme.background,
            titleContentColor = MaterialTheme.colorScheme.onBackground,
        ),
    )
}

internal sealed interface ExitCheck {
    data object Unchecked : ExitCheck
    data object Checking : ExitCheck
    data object NoReply : ExitCheck
    data object CouldNotCheck : ExitCheck
    data class Replied(val roundTripMs: Long) : ExitCheck
}

@Composable
internal fun ConnectionHome(
    profile: ClientProfile?,
    profileName: String?,
    connection: ConnectionState,
    backgroundFailureName: String?,
    metrics: DriveMetrics?,
    exitCheck: ExitCheck,
    onCheckExit: () -> Unit,
    onConnect: () -> Unit,
    onDisconnect: () -> Unit,
    onReplace: () -> Unit,
    onOpenProfiles: () -> Unit,
    onOpenSettings: () -> Unit,
    onOpenVpnApps: () -> Unit,
    onOpenLogs: () -> Unit,
) {
    val mode = if (connection.running) connection.mode.ifBlank {
        profile?.connectionMode ?: ClientProfile.CONNECTION_MODE_VPN
    } else profile?.connectionMode ?: ClientProfile.CONNECTION_MODE_VPN
    val proxy = mode == ClientProfile.CONNECTION_MODE_PROXY
    val failed = connection.phase == ConnectionPhase.FAILED
    val disconnecting = connection.phase == ConnectionPhase.DISCONNECTING
    val googleRecovery = if (failed) googleAccessRecovery(connection.message) else null
    val accessExpired = googleRecovery == GoogleAccessRecovery.RenewGrant
    val missingApps = !proxy && !connection.running && profile?.vpnAppMode == ClientProfile.VPN_APPS_SELECTED &&
        (profile.vpnAllowedApps.isEmpty() || (failed && isVpnAppSelectionFailure(connection.message)))
    val title = when (connection.phase) {
        ConnectionPhase.CONNECTED -> if (proxy) "Proxy running" else "VPN running"
        ConnectionPhase.CONNECTING -> "Connecting…"
        ConnectionPhase.DISCONNECTING -> "Disconnecting…"
        ConnectionPhase.FAILED -> when {
            accessExpired -> "Profile needs renewal"
            googleRecovery != null -> GOOGLE_ACCESS_NEEDS_ATTENTION
            missingApps -> "No VPN apps available"
            else -> "Couldn’t connect"
        }
        ConnectionPhase.IDLE -> "Disconnected"
    }
    val detail = when {
        disconnecting -> if (proxy) "Stopping the local proxy." else "Stopping the Android VPN."
        connection.running && profile == null -> "The active profile is missing. Disconnect and add it again."
        connection.phase == ConnectionPhase.CONNECTING -> if (proxy) "Starting the local proxy." else "Starting the Android VPN."
        connection.phase == ConnectionPhase.CONNECTED -> when {
            proxy && profile?.shareLan == true -> "The proxy is listening on your local network."
            proxy -> "The proxy is listening on this device."
            else -> "Android has started the VPN."
        }
        accessExpired -> "Ask the person running the exit for a renewed profile, then paste it here."
        googleRecovery != null -> "Ask the exit operator to check Google access. If they send a profile for a new Google setup, add it in Profiles."
        missingApps -> "Choose at least one installed app to use this VPN."
        failed -> connectionFailureHint(connection.message, proxy)
        connection.message == "VPN permission was not granted" -> "Allow Android’s VPN request when you connect."
        else -> "Skirk isn’t routing traffic."
    }

    val status: @Composable () -> Unit = {
        val live = connection.phase == ConnectionPhase.CONNECTED
        val colors = MaterialTheme.colorScheme
        Column {
            Surface(
                shape = RoundedCornerShape(14.dp),
                color = when {
                    failed -> colors.errorContainer
                    live -> colors.primaryContainer
                    else -> colors.surfaceContainerHigh
                },
                contentColor = when {
                    failed -> colors.onErrorContainer
                    live -> colors.onPrimaryContainer
                    else -> colors.onSurfaceVariant
                },
            ) {
                Box(Modifier.size(52.dp), contentAlignment = Alignment.Center) {
                    if (connection.phase == ConnectionPhase.CONNECTING || disconnecting) {
                        CircularProgressIndicator(Modifier.size(22.dp), strokeWidth = 2.dp)
                    } else {
                        Icon(when {
                            failed -> Icons.Outlined.ErrorOutline
                            proxy -> Icons.Outlined.WifiTethering
                            else -> Icons.Outlined.Shield
                        }, contentDescription = null, modifier = Modifier.size(26.dp))
                    }
                }
            }
            Spacer(Modifier.height(16.dp))
            Column(
                modifier = Modifier.semantics(mergeDescendants = true) { liveRegion = LiveRegionMode.Polite },
                verticalArrangement = Arrangement.spacedBy(6.dp),
            ) {
                Text(title, style = MaterialTheme.typography.headlineLarge, modifier = Modifier.semantics { heading() })
                Text(detail, color = colors.onSurfaceVariant, style = MaterialTheme.typography.bodyLarge)
            }
            Spacer(Modifier.height(20.dp))
            if (connection.running) {
                OutlinedButton(onClick = onDisconnect, enabled = !disconnecting, modifier = Modifier.fillMaxWidth().heightIn(min = 56.dp)) {
                    Icon(Icons.Rounded.PowerSettingsNew, contentDescription = null, modifier = Modifier.size(20.dp))
                    Spacer(Modifier.width(8.dp))
                    Text(when {
                        disconnecting -> "Disconnecting…"
                        connection.phase == ConnectionPhase.CONNECTING -> "Cancel connection"
                        else -> "Disconnect"
                    })
                }
            } else {
                val action = when {
                    accessExpired -> onReplace
                    googleRecovery != null -> onOpenProfiles
                    missingApps -> onOpenVpnApps
                    else -> onConnect
                }
                Button(onClick = action, enabled = profile != null, modifier = Modifier.fillMaxWidth().heightIn(min = 56.dp)) {
                    if (googleRecovery == null && !missingApps) {
                        Icon(Icons.Rounded.PowerSettingsNew, contentDescription = null, modifier = Modifier.size(20.dp))
                        Spacer(Modifier.width(8.dp))
                    }
                    Text(when {
                        accessExpired -> "Replace profile"
                        googleRecovery != null -> "Manage profiles"
                        missingApps -> "Choose VPN apps"
                        failed -> "Retry connection"
                        else -> "Connect"
                    })
                }
            }
            if (googleRecovery == GoogleAccessRecovery.ContactExitOperator && profile != null) {
                TextButton(onClick = onConnect) { Text("Retry connection") }
            }
            if (failed && !accessExpired) TextButton(onClick = onOpenLogs) { Text("View connection logs") }
            if (metrics?.backoffActive == true) {
                StatusNotice("Google Drive is limiting requests. Traffic may pause.")
            }
            if ((metrics?.recentUdpRemoteExitLimitHits ?: 0L) > 0L) {
                StatusNotice("The exit is full for new UDP traffic. Retry the call when another device disconnects.")
            } else if ((metrics?.recentUdpAdmissionLimitHits ?: 0L) > 0L) {
                StatusNotice("Skirk reached this phone’s UDP connection limit. Close an unused call or app and try again.")
            }
        }
    }
    val details: @Composable () -> Unit = {
        Column {
            HorizontalDivider()
            if (connection.phase == ConnectionPhase.CONNECTED) {
                ExitStatus(exitCheck, metrics?.udpEvidence ?: UdpEvidence.Unknown, profile != null, onCheckExit)
                HorizontalDivider()
            }
            SettingsRow(
                title = "Profile",
                detail = profileName ?: "Active profile unavailable",
                icon = Icons.Outlined.AccountCircle,
                onClick = onOpenProfiles,
                maxDetailLines = 2,
            )
            HorizontalDivider()
            if (profile != null) {
                SettingsRow(
                    title = "Connection settings",
                    detail = "${routingSummary(profile, proxy)} · ${googleRouteLabel(profile.routeMode)}",
                    icon = Icons.Outlined.Settings,
                    onClick = onOpenSettings,
                )
                HorizontalDivider()
            }
            if (backgroundFailureName != null) {
                Text(
                    "Last attempt with $backgroundFailureName failed. Select it in Profiles to see how to recover.",
                    modifier = Modifier.padding(top = 12.dp),
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    style = MaterialTheme.typography.bodyMedium,
                )
            }
        }
    }
    BoxWithConstraints(Modifier.fillMaxSize()) {
        val twoColumns = maxWidth >= 700.dp && LocalDensity.current.fontScale <= 1.3f
        if (twoColumns) {
            Row(
                Modifier.fillMaxSize().verticalScroll(rememberScrollState()).padding(horizontal = 32.dp, vertical = 22.dp),
                horizontalArrangement = Arrangement.spacedBy(44.dp),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                Box(Modifier.weight(1f)) { status() }
                Box(Modifier.weight(1f)) { details() }
            }
        } else {
            Column(
                Modifier.fillMaxSize().verticalScroll(rememberScrollState()).padding(horizontal = 24.dp, vertical = 20.dp),
                verticalArrangement = Arrangement.spacedBy(28.dp),
            ) {
                status()
                details()
            }
        }
    }
}

internal fun vpnAppsSummary(profile: ClientProfile): String {
    val excluded = profile.vpnAppMode == ClientProfile.VPN_APPS_EXCLUDED
    if (profile.vpnAppMode == ClientProfile.VPN_APPS_ALL || (excluded && profile.vpnExcludedApps.isEmpty())) return "All apps"
    val count = if (excluded) profile.vpnExcludedApps.size else profile.vpnAllowedApps.size
    val apps = if (count == 1) "1 app" else "$count apps"
    return if (excluded) "All except $apps" else apps
}

private fun routingSummary(profile: ClientProfile, proxy: Boolean): String = when {
    !proxy -> "VPN · ${vpnAppsSummary(profile)}"
    profile.shareLan -> "LAN proxy"
    else -> "Local proxy"
}

private fun connectionFailureHint(message: String, proxy: Boolean): String {
    val text = message.lowercase()
    return when {
        message == DRIVE_CLOCK_SKEW_HINT || message == DRIVE_CLOCK_UNVERIFIED_HINT -> message
        "permission was revoked" in text -> "Android revoked the VPN permission. Try connecting again to allow it."
        "foreground service timeout" in text ->
            "Android stopped the proxy at its background time limit. Reconnect or try VPN mode."
        "address already in use" in text || "is still in use on" in text ->
            "A local port is in use. Close the other app or open ${if (proxy) "Proxy ports" else "Local ports"} in Connection settings."
        "timeout" in text || "deadline exceeded" in text ->
            "Skirk timed out. Check your network and the selected Google route, then retry."
        "not found" in text || "mailbox missing" in text ->
            "Skirk couldn’t find a required resource. Check the logs or ask the exit operator to check the Drive mailbox."
        else -> "Try again. If the problem repeats, open the connection logs."
    }
}

@Composable
private fun StatusNotice(text: String) {
    Text(text, modifier = Modifier.padding(top = 12.dp).semantics { liveRegion = LiveRegionMode.Polite },
        color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodyMedium)
}

@Composable
private fun ExitStatus(check: ExitCheck, udpEvidence: UdpEvidence, enabled: Boolean, onCheck: () -> Unit) {
    val palette = MaterialTheme.colorScheme
    val title = when (check) {
        ExitCheck.Unchecked -> "Exit TCP not checked"
        ExitCheck.Checking -> "Checking exit TCP…"
        ExitCheck.NoReply -> "No verified TCP reply"
        ExitCheck.CouldNotCheck -> "Exit TCP check unavailable"
        is ExitCheck.Replied -> "Exit TCP verified"
    }
    val detail = when (check) {
        ExitCheck.Unchecked -> if (enabled) "Check for a TCP reply from the exit through Google Drive." else "Reconnect with a saved profile to check the exit."
        ExitCheck.Checking -> "Waiting for an authenticated TCP reply."
        ExitCheck.NoReply -> "Check that the exit is running, then try again."
        ExitCheck.CouldNotCheck -> "Reconnect and try again."
        is ExitCheck.Replied -> {
            val seconds = NumberFormat.getNumberInstance().apply { maximumFractionDigits = 2 }
                .format(check.roundTripMs.toDouble() / 1000)
            "Authenticated reply in $seconds s."
        }
    }
    Column(Modifier.fillMaxWidth().padding(top = 12.dp, bottom = 4.dp)) {
        Column(Modifier.semantics(mergeDescendants = true) { liveRegion = LiveRegionMode.Polite }) {
            Row(horizontalArrangement = Arrangement.spacedBy(6.dp), verticalAlignment = Alignment.CenterVertically) {
                if (check is ExitCheck.Replied) {
                    Icon(Icons.Rounded.CheckCircle, contentDescription = null,
                        tint = palette.onPrimaryContainer, modifier = Modifier.size(18.dp))
                }
                Text(title, style = MaterialTheme.typography.titleMedium)
            }
            Spacer(Modifier.height(2.dp))
            Text(detail, color = palette.onSurfaceVariant, style = MaterialTheme.typography.bodyMedium)
        }
        if (enabled && check != ExitCheck.Checking) {
            TextButton(onClick = onCheck, modifier = Modifier.align(Alignment.Start).heightIn(min = 48.dp)) {
                Text(if (check == ExitCheck.Unchecked) "Check exit" else "Check again")
            }
        } else {
            Spacer(Modifier.height(8.dp))
        }
        val udpStatus = when (udpEvidence) {
            UdpEvidence.Unknown -> "UDP status unavailable."
            UdpEvidence.NotEnabled -> "UDP preview is off for this connection."
            UdpEvidence.NoReturnObserved -> "UDP preview is configured. Skirk hasn’t received UDP from the exit yet."
            UdpEvidence.ReturnObserved -> "Skirk received UDP from the exit on this connection."
        }
        Text(udpStatus, modifier = Modifier.semantics { liveRegion = LiveRegionMode.Polite },
            color = palette.onSurfaceVariant, style = MaterialTheme.typography.bodyMedium)
    }
}
