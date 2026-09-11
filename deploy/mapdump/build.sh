#!/bin/bash
# Build mapdump.jar, which recovers the Alexa credential from MAP's store.
#
# Two build inputs are needed and are deliberately not committed: 46MB between
# them, and only needed to touch this one file. Both are public, and both were
# checked against the sha1 their own index publishes:
#
#   ANDROID_JAR   android.jar for API 22, from the SDK platform package
#                 https://dl.google.com/android/repository/android-22_r02.zip
#                 sha1 5d1bd10fea962b216a0dece1247070164760a9fc, then unzip
#                 android-5.1.1/android.jar out of it
#
#   R8_JAR        any jar carrying com.android.tools.r8.D8. The SDK ships one at
#                 build-tools/<version>/lib/d8.jar, which is what README.md and
#                 CI use; standalone, it is
#                 https://dl.google.com/dl/android/maven2/com/android/tools/r8/
#                 9.4.14/r8-9.4.14.jar  (.sha1 sits beside it)
#
# The versions are what the index offered rather than anything this needs: D8
# only has to dex one class. One JDK does both steps, but not just any JDK: javac
# dropped -source 1.7 in JDK 20, and D8 wants 11 or newer, so it has to be one of
# 11 through 19. CI pins 17, because the runner default drifts.
#
#   ANDROID_JAR=~/android.jar R8_JAR=~/r8.jar deploy/mapdump/build.sh
set -e
cd "$(dirname "$0")"

: "${ANDROID_JAR:?set ANDROID_JAR to an API 22 android.jar}"
: "${R8_JAR:?set R8_JAR to an r8.jar}"

rm -rf classes out mapdump.jar
mkdir -p classes out
javac -source 1.7 -target 1.7 -nowarn -bootclasspath "$ANDROID_JAR" -d classes MapDump.java
java -cp "$R8_JAR" com.android.tools.r8.D8 --lib "$ANDROID_JAR" --min-api 22 --output out classes/*.class
(cd out && zip -q ../mapdump.jar classes.dex)
rm -rf classes out
ls -l mapdump.jar
